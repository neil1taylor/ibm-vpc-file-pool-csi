import React, { useEffect, useRef, useState } from 'react';
import { useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Link } from 'react-router-dom';
import {
  Alert,
  Button,
  Content,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Form,
  FormGroup,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Radio,
  Spinner,
  TextInput,
} from '@patternfly/react-core';
import { StorageClassModel } from '../../models';
import { GatewayError, formatSize, momentLabel } from '../../utils/backup-browse';
import {
  MAX_PATHS,
  POLL_MS,
  PHASE_ORDER,
  Restore,
  SelectionMap,
  buildRestoreRequest,
  claimLink,
  createRestore,
  defaultClaimName,
  getRestore,
  isEnded,
  pathText,
  phaseText,
  restoreErrorText,
  selectionSummary,
  uncovered,
  suggestName,
  validateRestoreForm,
} from '../../utils/backup-file-restore';
import { StorageClass } from '../../utils/k8s-objects';
import { RESTORE_FILES_GUIDE } from '../../utils/help-links';
import HelpLink from '../common/HelpLink';

interface Props {
  policy: string;
  /** The namespace and name of the backed-up claim. */
  namespace: string;
  pvc: string;
  /** RFC 3339, or empty for the latest copy. */
  moment: string;
  /** True when the files are inside a VM disk: the paths begin with a file system of the guest. */
  guest?: boolean;
  selected: SelectionMap;
  onClose: () => void;
}

/** How many chosen paths the dialog lists. The rest are counted. */
const LISTED = 10;
/** A progress check that fails this many times in a row ends the polling. The restore itself goes on. */
const MAX_POLL_FAILURES = 3;

const plural = (n: number, one: string, many: string): string => `${n} ${n === 1 ? one : many}`;

/** The result of a restore, in the words of the page: counts and size. */
export function resultText(r: Restore): string {
  const parts = [plural(r.files, 'file', 'files'), plural(r.dirs, 'folder', 'folders'), plural(r.links, 'link', 'links')];
  return `${parts.join(', ')}, ${formatSize(r.bytes)}`;
}

/**
 * Restores the files and folders chosen in the browser into a new claim, or into a new folder of an existing
 * claim, through the backup gateway. It never writes over anything. After the person confirms, it follows the
 * restore by short requests until the restore is Done or Failed. Names are only ever shown as text.
 */
const RestoreFilesDialog: React.FC<Props> = ({ policy, namespace, pvc, moment, guest, selected, onClose }) => {
  const [targetNamespace, setTargetNamespace] = useState(namespace);
  const [targetName, setTargetName] = useState(defaultClaimName(pvc));
  const [existing, setExisting] = useState(false);
  const [sizeGB, setSizeGB] = useState('');
  const [storageClassName, setStorageClassName] = useState('');
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<GatewayError | undefined>();
  const [restore, setRestore] = useState<Restore | undefined>();
  const [pollError, setPollError] = useState<GatewayError | undefined>();

  const validNamespace = !validateRestoreForm({ targetNamespace, targetName: 'x', sizeGB: '', existing }).targetNamespace;
  const rightsNamespace = validNamespace ? targetNamespace : namespace;
  const [canCreate] = useAccessReview({ group: '', resource: 'persistentvolumeclaims', verb: 'create', namespace: rightsNamespace });
  const [canUpdate] = useAccessReview({ group: '', resource: 'persistentvolumeclaims', verb: 'update', namespace: rightsNamespace });
  const [canPods] = useAccessReview({ group: '', resource: 'pods', verb: 'create', namespace: rightsNamespace });
  const [storageClasses] = useK8sWatchResource<StorageClass[]>({
    groupVersionKind: { group: StorageClassModel.apiGroup, version: StorageClassModel.apiVersion, kind: StorageClassModel.kind },
    isList: true,
  });

  const existingMissing: string[] = [];
  if (!canUpdate) existingMissing.push(`update on persistentvolumeclaims in ${rightsNamespace}`);
  if (!canPods) existingMissing.push(`create on pods in ${rightsNamespace}`);
  const existingAllowed = canCreate && existingMissing.length === 0;
  // A choice of "existing" that the rights no longer allow (the namespace was changed) falls back to a new claim.
  const useExisting = existing && existingAllowed;

  const summary = selectionSummary(selected);
  const errors = validateRestoreForm({ targetNamespace, targetName, sizeGB, existing: useExisting });
  const tooMany = uncovered(selected).length > MAX_PATHS;
  const canStart = canCreate && !tooMany && selected.size > 0 && Object.keys(errors).length === 0 && !busy;
  const target = { namespace, targetNamespace, existing: useExisting };

  const confirm = () => {
    setBusy(true);
    setRefusal(undefined);
    createRestore(buildRestoreRequest({ policy, namespace, pvc, moment, selected, targetNamespace, targetName, existing: useExisting, sizeGB, storageClassName }))
      .then((r) => setRestore(r))
      .catch((e: GatewayError) => setRefusal(e))
      .finally(() => setBusy(false));
  };

  // Follow the restore until it ends. One short request at a time; closing the dialog stops it.
  const restoreName = restore?.name;
  const ended = restore ? isEnded(restore.phase) : false;
  const failures = useRef(0);
  useEffect(() => {
    if (!restoreName || ended) return undefined;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const tick = () => {
      getRestore(restoreName)
        .then((r) => {
          if (cancelled) return;
          failures.current = 0;
          setPollError(undefined);
          setRestore(r);
          if (!isEnded(r.phase)) timer = setTimeout(tick, POLL_MS);
        })
        .catch((e: GatewayError) => {
          if (cancelled) return;
          failures.current++;
          if (failures.current >= MAX_POLL_FAILURES || e.status === 404 || e.status === 403) {
            setPollError(e);
            return;
          }
          timer = setTimeout(tick, POLL_MS);
        });
    };
    timer = setTimeout(tick, POLL_MS);
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [restoreName, ended]);

  const where = useExisting
    ? `a new folder at the top of the existing claim ${targetNamespace}/${targetName}`
    : `a new claim ${targetNamespace}/${targetName}`;
  const paths = uncovered(selected).map((s) => pathText(s.path)).sort();

  const form = (
    <Form
      onSubmit={(e) => {
        e.preventDefault();
        if (canStart) confirm();
      }}
    >
      <Alert variant="info" isInline title="What will be written">
        <p>
          {[
            summary.files ? plural(summary.files, 'file', 'files') + (summary.fileBytes ? ` (${formatSize(summary.fileBytes)})` : '') : '',
            summary.folders ? plural(summary.folders, 'folder', 'folders') : '',
            summary.links ? plural(summary.links, 'link', 'links') : '',
          ]
            .filter(Boolean)
            .join(', ')}{' '}
          from the backup of {namespace}/{pvc}, {moment ? momentLabel(moment) : 'Latest copy'}, into {where}. A folder is restored with everything in
          it. A link is restored as a link and is not followed. The folders above each file are kept: /var/www/index.php is written as
          /var/www/index.php.
        </p>
        <ul style={{ marginTop: 8 }}>
          {paths.slice(0, LISTED).map((p) => (
            <li key={p}>{p}</li>
          ))}
        </ul>
        {paths.length > LISTED && <p>and {paths.length - LISTED} more</p>}
      </Alert>
      <Alert variant="warning" isInline title="Nothing is overwritten">
        A restore never writes over a file or a claim that exists, and never puts a file back in its old place. Your pods keep running with what they
        have.
      </Alert>
      {guest && (
        <Alert variant="info" isInline title="Files from inside a VM disk">
          The files are read from the disk without starting the VM. The name of the file system stays at the top of what is written (for example
          rhel-root/etc/hosts), and you copy the files into the VM from the new claim or folder. This can take long for many small files. The next
          backup of the disk waits while the restore runs.
        </Alert>
      )}
      {!moment && !guest && (
        <Alert variant="warning" isInline title="You chose the Latest copy">
          The next backup replaces files in it one at a time, so a file can change while it is copied. Go back and choose a moment to restore a copy
          that stays as it is.
        </Alert>
      )}
      {tooMany && (
        <Alert variant="danger" isInline title="Too many files and folders chosen">
          A restore takes at most {MAX_PATHS} of them. Choose a folder that holds them instead.
        </Alert>
      )}
      {!canCreate && (
        <Alert variant="danger" isInline title="You may not make a claim here">
          You need the right to create persistentvolumeclaims in namespace {rightsNamespace}.
        </Alert>
      )}
      <FormGroup role="radiogroup" isInline={false} fieldId="restore-target" label="Where the files go">
        <Radio
          id="restore-target-new"
          name="restore-target"
          label="A new claim"
          description="The default. A claim is made, sized to the files, and held back from pods until the copy ends."
          isChecked={!useExisting}
          onChange={() => setExisting(false)}
        />
        <Radio
          id="restore-target-existing"
          name="restore-target"
          label="A new folder in an existing claim"
          description={
            existingAllowed
              ? 'The files go into a new folder at the top of the claim. Nothing else in the claim changes.'
              : `Not available to you: you also need ${existingMissing.length ? existingMissing.join(' and ') : `create on persistentvolumeclaims in ${rightsNamespace}`}.`
          }
          isChecked={useExisting}
          isDisabled={!existingAllowed}
          onChange={() => setExisting(true)}
        />
      </FormGroup>
      <FormGroup label="Namespace" fieldId="restore-namespace" isRequired>
        <TextInput id="restore-namespace" aria-label="Namespace" value={targetNamespace} onChange={(_e, v) => setTargetNamespace(v)} />
        {errors.targetNamespace && (
          <FormHelperText>
            <HelperText>
              <HelperTextItem variant="error">{errors.targetNamespace}</HelperTextItem>
            </HelperText>
          </FormHelperText>
        )}
      </FormGroup>
      <FormGroup label="Claim name" fieldId="restore-name" isRequired>
        <TextInput id="restore-name" aria-label="Claim name" value={targetName} onChange={(_e, v) => setTargetName(v)} />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={errors.targetName ? 'error' : 'default'}>
              {errors.targetName || (useExisting ? 'The claim that gets the new folder. It must exist.' : 'The new claim. It must not exist yet.')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>
      {!useExisting && (
        <>
          <FormGroup label="Size in GiB" fieldId="restore-size">
            <TextInput id="restore-size" aria-label="Size in GiB" value={sizeGB} placeholder="Automatic" onChange={(_e, v) => setSizeGB(v)} />
            <FormHelperText>
              <HelperText>
                <HelperTextItem variant={errors.sizeGB ? 'error' : 'default'}>
                  {errors.sizeGB || 'Leave it empty and the size of the chosen files is measured first: that size plus 20%, at least 1 GiB.'}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>
          <FormGroup label="Storage class" fieldId="restore-class">
            <FormSelect id="restore-class" aria-label="Storage class" value={storageClassName} onChange={(_e, v) => setStorageClassName(v)}>
              <FormSelectOption value="" label="Same as the backed-up claim" />
              {(storageClasses || []).map((c) => (
                <FormSelectOption key={c.metadata?.name} value={c.metadata?.name} label={c.metadata?.name || ''} />
              ))}
            </FormSelect>
          </FormGroup>
        </>
      )}
      {refusal && (
        <Alert variant="danger" isInline title="The restore could not be started">
          <p>{restoreErrorText(refusal, target)}</p>
          {refusal.status === 409 && (
            <Button variant="link" isInline onClick={() => setTargetName(suggestName(targetName))}>
              {`Use ${suggestName(targetName)}`}
            </Button>
          )}
        </Alert>
      )}
    </Form>
  );

  const progress = (r: Restore) => {
    const failed = r.phase === 'Failed';
    const done = r.phase === 'Done';
    const step = PHASE_ORDER.indexOf(r.phase);
    return (
      <>
        <Alert
          variant={failed ? 'danger' : done ? 'success' : 'info'}
          isInline
          title={phaseText(r.phase)}
          customIcon={!failed && !done ? <Spinner size="md" aria-label="Working" /> : undefined}
        >
          {!failed && !done && step >= 0 && <p>{`Step ${step + 1} of ${PHASE_ORDER.length}. This page asks again every 2 seconds. You can close it: the restore goes on.`}</p>}
          {r.message && <p>{r.message}</p>}
          {failed && r.existing === false && (
            <p>A claim that was made for this restore stays held back from pods. Delete it, and ask again.</p>
          )}
        </Alert>
        {pollError && (
          <Alert variant="warning" isInline title="This page cannot follow the restore any more" style={{ marginTop: 16 }}>
            <p>{restoreErrorText(pollError, target)}</p>
            <p>The restore is named {r.name} and goes on without this page.</p>
          </Alert>
        )}
        {done && (
          <DescriptionList isHorizontal style={{ marginTop: 16 }}>
            <DescriptionListGroup>
              <DescriptionListTerm>Claim</DescriptionListTerm>
              <DescriptionListDescription>
                <Link to={claimLink(r.targetNamespace, r.targetName)}>{`${r.targetNamespace}/${r.targetName}`}</Link>
              </DescriptionListDescription>
            </DescriptionListGroup>
            {r.folder && (
              <DescriptionListGroup>
                <DescriptionListTerm>Folder in the claim</DescriptionListTerm>
                <DescriptionListDescription>{r.folder}</DescriptionListDescription>
              </DescriptionListGroup>
            )}
            <DescriptionListGroup>
              <DescriptionListTerm>Restored</DescriptionListTerm>
              <DescriptionListDescription>{resultText(r)}</DescriptionListDescription>
            </DescriptionListGroup>
            {(r.skipped > 0 || r.unreadable > 0) && (
              <DescriptionListGroup>
                <DescriptionListTerm>Left out</DescriptionListTerm>
                <DescriptionListDescription>
                  {`${plural(r.skipped, 'special file', 'special files')} (devices, pipes, sockets) and ${plural(r.unreadable, 'file', 'files')} the reader could not open`}
                </DescriptionListDescription>
              </DescriptionListGroup>
            )}
            <DescriptionListGroup>
              <DescriptionListTerm>Moment</DescriptionListTerm>
              <DescriptionListDescription>
                {r.latest || !r.restoredMoment ? 'Latest copy' : momentLabel(r.restoredMoment)}
              </DescriptionListDescription>
            </DescriptionListGroup>
          </DescriptionList>
        )}
      </>
    );
  };

  return (
    <Modal variant={ModalVariant.medium} isOpen onClose={onClose} aria-label="Restore selected files">
      <ModalHeader title="Restore selected files" />
      <ModalBody>
        <Content component="p">
          <HelpLink href={RESTORE_FILES_GUIDE} label="Help with restoring files" testId="restore-help-link" />
        </Content>
        {restore ? progress(restore) : form}
      </ModalBody>
      <ModalFooter>
        {!restore && (
          <Button variant="primary" onClick={confirm} isDisabled={!canStart} isLoading={busy}>
            Restore
          </Button>
        )}
        <Button variant={restore ? 'primary' : 'link'} onClick={onClose}>
          {restore ? 'Close' : 'Cancel'}
        </Button>
      </ModalFooter>
    </Modal>
  );
};

export default RestoreFilesDialog;
