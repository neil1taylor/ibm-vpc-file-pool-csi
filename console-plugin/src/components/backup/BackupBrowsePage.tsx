import React, { useEffect, useMemo, useRef, useState } from 'react';
import { Link, useHistory, useLocation, useParams } from 'react-router-dom';
import {
  Alert,
  Breadcrumb,
  BreadcrumbItem,
  Bullseye,
  Button,
  Card,
  CardBody,
  Checkbox,
  FormSelect,
  FormSelectOption,
  Label,
  PageSection,
  Spinner,
  Split,
  SplitItem,
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarGroup,
  ToolbarItem,
} from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { ROUTES } from '../../constants';
import {
  BrowseEntry,
  GatewayError,
  browsePath,
  canDownload,
  downloadUrl,
  entryKind,
  entryText,
  escapeName,
  formatSize,
  getInfo,
  fromB64,
  gatewayErrorText,
  linkText,
  listFolder,
  listMoments,
  momentLabel,
  momentToISO,
  parseBrowseQuery,
} from '../../utils/backup-browse';
import {
  SelectionMap,

  canSelect,
  selectionKey,
  selectionSummary,
  toggleSelected,
} from '../../utils/backup-file-restore';
import RestoreFilesDialog from './RestoreFilesDialog';
import { useBrowseSession } from '../../utils/use-browse-session';
import { BROWSE_GUIDE } from '../../utils/help-links';
import CardHelpTitle from '../common/CardHelpTitle';
import HelpLink from '../common/HelpLink';

const timeText = (iso: string): string => (iso.length >= 19 ? `${iso.slice(0, 10)} ${iso.slice(11, 19)} UTC` : iso);

const isEnded = (e: GatewayError): boolean => e.status === 410 || (e.status === 404 && e.message === 'no such session');

/**
 * Looks inside one backup of a volume: folders, files, links and the moment to look at, with downloads of a
 * file or a folder (as a tar). It only reads. Every call goes to the backup gateway with the person's own
 * token, and the gateway checks the person's right to read the backups of the claim's namespace each time.
 */
const BackupBrowsePage: React.FC = () => {
  const { name } = useParams<{ name: string }>();
  const history = useHistory();
  const location = useLocation();
  const { namespace, pvc, moment } = useMemo(() => parseBrowseQuery(location.search), [location.search]);
  const claim = `${namespace}/${pvc}`;

  const session = useBrowseSession(namespace ? { policy: name, namespace, pvc, ...(moment ? { moment } : {}) } : undefined);
  const sessionName = session.status === 'ready' ? session.session : undefined;
  const guest = !!session.guest;

  // The folder: base64 names exactly as the gateway sent them. Names are never decoded and encoded again.
  const [path, setPath] = useState<string[]>([]);
  const [entries, setEntries] = useState<BrowseEntry[]>([]);
  const [next, setNext] = useState<string | undefined>();
  const [loading, setLoading] = useState(false);
  const [listError, setListError] = useState<GatewayError | undefined>();
  const [moments, setMoments] = useState<string[]>([]);
  const [notices, setNotices] = useState<string[]>([]);
  const [selected, setSelected] = useState<SelectionMap>(() => new Map());
  const [restoring, setRestoring] = useState(false);
  const request = useRef(0);
  const dropPending = () => {
    request.current++;
  };

  // A new session, a new moment or a new claim starts again at the root.
  useEffect(() => {
    setPath((p) => (p.length ? [] : p));
  }, [sessionName]);

  // The choice belongs to one claim at one moment. It survives a folder change and an ended session
  // (the restore does not use the session), and it is dropped when the claim or the moment changes.
  const choiceOf = `${claim}|${moment}`;
  useEffect(() => {
    setSelected((old) => (old.size ? new Map() : old));
    setRestoring(false);
  }, [choiceOf]);

  const load = (cursor?: string) => {
    if (!sessionName) return;
    const id = ++request.current;
    setLoading(true);
    setListError(undefined);
    listFolder(sessionName, path, cursor)
      .then((page) => {
        if (id !== request.current) return;
        setEntries((old) => (cursor ? [...old, ...page.entries] : page.entries));
        setNext(page.next || undefined);
      })
      .catch((e: GatewayError) => {
        if (id === request.current) setListError(e);
      })
      .finally(() => {
        if (id === request.current) setLoading(false);
      });
  };

  useEffect(() => {
    setEntries([]);
    setNext(undefined);
    load();
    return dropPending;
    // load reads path and sessionName, which are the dependencies.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionName, path]);

  useEffect(() => {
    if (!sessionName) return undefined;
    let cancelled = false;
    listMoments(sessionName)
      .then((m) => {
        if (!cancelled) setMoments(m);
      })
      .catch(() => {
        if (!cancelled) setMoments([]);
      });
    return () => {
      cancelled = true;
    };
  }, [sessionName]);

  // A session on a VM disk says things a person should know, such as that a copy was taken while the VM ran.
  useEffect(() => {
    if (!sessionName || !guest) {
      setNotices([]);
      return undefined;
    }
    let cancelled = false;
    getInfo(sessionName)
      .then((info) => {
        if (!cancelled) setNotices(info.notices || []);
      })
      .catch(() => {
        if (!cancelled) setNotices([]);
      });
    return () => {
      cancelled = true;
    };
  }, [sessionName, guest]);

  const momentOptions = useMemo(() => {
    const isos = moments.map(momentToISO).filter((m): m is string => !!m);
    isos.sort().reverse();
    if (moment && !isos.includes(moment)) isos.unshift(moment);
    return isos;
  }, [moments, moment]);

  const back = (
    <Link to={ROUTES.BACKUP_DETAIL.replace(':name', encodeURIComponent(name))}>Back to the backup policy {name}</Link>
  );

  const body = ((): React.ReactNode => {
    if (!namespace) {
      return (
        <Alert variant="info" isInline title="No volume chosen">
          Open a backup policy and choose Browse files on a volume.
        </Alert>
      );
    }
    if (session.status === 'error' && session.error) {
      return (
        <Alert variant="danger" isInline title="Cannot open this backup">
          {gatewayErrorText(session.error, namespace)}
        </Alert>
      );
    }
    const ended = session.status === 'ended' || (listError && isEnded(listError));
    if (ended) {
      return (
        <Alert
          variant="warning"
          isInline
          title="The session has ended"
          actionLinks={<Button variant="link" onClick={session.reopen}>Open again</Button>}
        >
          A session ends after 15 minutes without use, after 2 hours, or when someone closes it.
          {session.message ? ` ${session.message}` : ''}
        </Alert>
      );
    }
    if (session.status === 'starting') {
      return (
        <Bullseye style={{ minHeight: 160 }}>
          <div style={{ textAlign: 'center' }}>
            <Spinner size="lg" aria-label="Starting the reader" />
            {guest ? (
              <p>
                Starting the reader of the VM disk. This can take a few minutes: it waits for a backup copy that is being written, then
                starts a small virtual machine that reads the disk. The VM itself is not started.
              </p>
            ) : (
              <p>Starting the reader. This takes a few seconds.</p>
            )}
            {session.message && <p>{session.message}</p>}
          </div>
        </Bullseye>
      );
    }
    return listing();
  })();

  function listing(): React.ReactNode {
    const sessionId = sessionName as string;
    const rootLabel = guest ? 'VM disk' : 'Backup root';
    const pageChoosable = entries.filter(canSelect);
    const crumbs = path.map((seg, i) => ({ seg, label: escapeName(fromB64(seg)), upTo: path.slice(0, i + 1) }));
    return (
      <>
        <Toolbar>
          <ToolbarContent>
            <ToolbarItem>
              <Breadcrumb aria-label="Folders">
                {path.length === 0 ? (
                  <BreadcrumbItem isActive>{rootLabel}</BreadcrumbItem>
                ) : (
                  <BreadcrumbItem component="button" onClick={() => setPath([])}>
                    {rootLabel}
                  </BreadcrumbItem>
                )}
                {crumbs.map((c, i) =>
                  i === crumbs.length - 1 ? (
                    <BreadcrumbItem key={c.upTo.join('/')} isActive>
                      {c.label}
                    </BreadcrumbItem>
                  ) : (
                    <BreadcrumbItem key={c.upTo.join('/')} component="button" onClick={() => setPath(c.upTo)}>
                      {c.label}
                    </BreadcrumbItem>
                  ),
                )}
              </Breadcrumb>
            </ToolbarItem>
            {/* File actions: the choice made in the table (count, total, restore, clear) and the download of this folder. */}
            <ToolbarGroup align={{ default: 'alignEnd' }} data-testid="file-actions">
              <ToolbarItem>
                <SelectionSummary selected={selected} />
              </ToolbarItem>
              <ToolbarItem>
                <Button variant="primary" isDisabled={selected.size === 0} onClick={() => setRestoring(true)}>
                  Restore selected
                </Button>
              </ToolbarItem>
              <ToolbarItem>
                <Button variant="link" isDisabled={selected.size === 0} onClick={() => setSelected(new Map())}>
                  Clear
                </Button>
              </ToolbarItem>
              {!(guest && path.length === 0) && (
                <ToolbarItem>
                  <a href={downloadUrl(sessionId, 'tar', path)} download>
                    Download this folder as tar
                  </a>
                </ToolbarItem>
              )}
            </ToolbarGroup>
          </ToolbarContent>
        </Toolbar>
        {notices.map((n) => (
          <Alert key={n} variant="warning" isInline title={n} style={{ marginBottom: 16 }} />
        ))}
        {guest && path.length === 0 && (
          <Alert variant="info" isInline title="Choose a file system" style={{ marginBottom: 16 }}>
            These are the partitions and volumes inside the VM disk. A volume that is encrypted or has a file system that cannot be read here
            is listed with the reason. To get such a volume back, restore the whole disk.
          </Alert>
        )}
        {listError && !isEnded(listError) && (
          <Alert variant="danger" isInline title="Cannot list this folder" style={{ marginBottom: 16 }}>
            {gatewayErrorText(listError, namespace)}
          </Alert>
        )}
        {entries.length === 0 && !loading && !listError ? (
          <Alert variant="info" isInline title="This folder is empty" />
        ) : (
          <Table aria-label="Files and folders" variant="compact">
            <Thead>
              <Tr>
                <Th screenReaderText="Choose">
                  <Checkbox
                    id="choose-all"
                    aria-label="Choose all on this page"
                    isChecked={pageChoosable.length > 0 && pageChoosable.every((e) => selected.has(selectionKey([...path, e.name])))}
                    isDisabled={pageChoosable.length === 0}
                    onChange={(_e, on) =>
                      setSelected((old) => {
                        const out = new Map(old);
                        pageChoosable.forEach((e) => {
                          const key = selectionKey([...path, e.name]);
                          if (on) out.set(key, { path: [...path, e.name], entry: e });
                          else out.delete(key);
                        });
                        return out;
                      })
                    }
                  />
                </Th>
                <Th>Name</Th>
                <Th>Size</Th>
                <Th>Changed</Th>
                <Th>Note</Th>
                <Th>Download</Th>
              </Tr>
            </Thead>
            <Tbody>
              {entries.map((e) => (
                <EntryRow
                  key={e.name}
                  entry={e}
                  guest={guest}
                  isChosen={selected.has(selectionKey([...path, e.name]))}
                  onChoose={() => setSelected((old) => toggleSelected(old, { path: [...path, e.name], entry: e }))}
                  onOpen={() => setPath([...path, e.name])}
                  fileLink={downloadUrl(sessionId, 'file', [...path, e.name])}
                  tarLink={downloadUrl(sessionId, 'tar', [...path, e.name])}
                />
              ))}
            </Tbody>
          </Table>
        )}
        {loading && (
          <Bullseye style={{ minHeight: 60 }}>
            <Spinner size="md" aria-label="Loading" />
          </Bullseye>
        )}
        {next && !loading && (
          <Button variant="secondary" onClick={() => load(next)} style={{ marginTop: 12 }}>
            Load more
          </Button>
        )}
      </>
    );
  }

  return (
    <PageSection>
      <Split hasGutter style={{ marginBottom: 8 }}>
        <SplitItem isFilled>
          <Title headingLevel="h1">Browse files{namespace ? `: ${claim}` : ''}</Title>
        </SplitItem>
        <SplitItem>
          <HelpLink href={BROWSE_GUIDE} label="Help with browsing backups" testId="browse-help-link" />
        </SplitItem>
      </Split>
      <div style={{ marginBottom: 16 }}>{back}</div>
      {namespace && session.status !== 'error' && (
        <Card style={{ marginBottom: 16 }}>
          <CardHelpTitle helpKey="backupBrowseMoment" />
          <CardBody>
            <FormSelect
              id="browse-moment"
              aria-label="Moment"
              value={moment}
              onChange={(_e, v) => history.replace(browsePath(name, claim, v || undefined))}
              style={{ maxWidth: 360 }}
            >
              <FormSelectOption value="" label="Latest copy" />
              {momentOptions.map((m) => (
                <FormSelectOption key={m} value={m} label={momentLabel(m)} />
              ))}
            </FormSelect>
            {guest ? (
              <Alert variant="info" isInline isPlain title="The next backup of this disk waits while you look" style={{ marginTop: 8 }}>
                A VM disk is shown as it was at the moment you choose, without starting the VM. While this page is open the next backup copy of
                the disk is held back, for 2 hours at most. Close the page when you are done.
              </Alert>
            ) : (
              !moment && (
                <Alert variant="info" isInline isPlain title="The latest copy may change while you look" style={{ marginTop: 8 }}>
                  The next backup replaces files in it one at a time. Choose a moment to look at a copy that stays as it is.
                </Alert>
              )
            )}
          </CardBody>
        </Card>
      )}
      <Card>
        <CardHelpTitle helpKey="backupBrowseFiles" />
        <CardBody>{body}</CardBody>
      </Card>
      {restoring && (
        <RestoreFilesDialog
          policy={name}
          namespace={namespace}
          pvc={pvc}
          moment={moment}
          guest={guest}
          selected={selected}
          onClose={() => setRestoring(false)}
        />
      )}
    </PageSection>
  );
};

/** How many are chosen and how big, in words. A folder is counted without a size: it is measured when the restore runs. */
const SelectionSummary: React.FC<{ selected: SelectionMap }> = ({ selected }) => {
  if (selected.size === 0) return <span>Nothing chosen</span>;
  const s = selectionSummary(selected);
  const parts = [`${s.files} ${s.files === 1 ? 'file' : 'files'}${s.files ? ` (${formatSize(s.fileBytes)})` : ''}`];
  if (s.folders) parts.push(`${s.folders} ${s.folders === 1 ? 'folder' : 'folders'} with everything in it`);
  if (s.links) parts.push(`${s.links} ${s.links === 1 ? 'link' : 'links'}`);
  return (
    <span data-testid="selection-summary">
      <strong>{s.count} chosen</strong>: {parts.join(', ')}
    </span>
  );
};

interface RowProps {
  entry: BrowseEntry;
  guest?: boolean;
  isChosen: boolean;
  onChoose: () => void;
  onOpen: () => void;
  fileLink: string;
  tarLink: string;
}

/** One name of a folder. Names and link texts are put in as text and never as HTML. */
const EntryRow: React.FC<RowProps> = ({ entry, guest, isChosen, onChoose, onOpen, fileLink, tarLink }) => {
  const kind = entryKind(entry);
  const label = entryText(entry);
  let name: React.ReactNode = label;
  if (kind === 'folder' && entry.readable) {
    name = (
      <Button variant="link" isInline onClick={onOpen}>
        {label}
      </Button>
    );
  } else if (kind === 'link') {
    name = (
      <>
        {label} → <span>{linkText(entry)}</span>
      </>
    );
  }
  let note: React.ReactNode = '';
  if (!entry.readable) {
    note = (
      <>
        <Label color="orange" isCompact>
          {guest ? 'Cannot be read here' : 'Not readable'}
        </Label>
        {entry.note && <div>{entry.note}</div>}
      </>
    );
  } else if (entry.note) note = entry.note;
  else if (kind === 'link') note = <Label isCompact>Link, not followed</Label>;
  else if (kind === 'other') note = <Label isCompact>Special file, not shown</Label>;
  return (
    <Tr>
      <Td dataLabel="Choose">
        {canSelect(entry) && <Checkbox id={`choose-${entry.name}`} aria-label={`Choose ${label}`} isChecked={isChosen} onChange={onChoose} />}
      </Td>
      <Td dataLabel="Name">{name}</Td>
      <Td dataLabel="Size">{kind === 'file' ? formatSize(entry.size) : ''}</Td>
      <Td dataLabel="Changed">{timeText(entry.mtime)}</Td>
      <Td dataLabel="Note">{note}</Td>
      <Td dataLabel="Download">
        {canDownload(entry) && (
          <a href={kind === 'folder' ? tarLink : fileLink} download>
            {kind === 'folder' ? 'Download as tar' : 'Download'}
          </a>
        )}
      </Td>
    </Tr>
  );
};

export default BackupBrowsePage;
