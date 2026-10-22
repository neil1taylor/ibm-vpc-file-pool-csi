import React from 'react';
import { Link, useHistory } from 'react-router-dom';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import {
  Alert,
  Card,
  CardBody,
  CardTitle,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Label,
  Stack,
  StackItem,
} from '@patternfly/react-core';
import { BackupPolicy } from '../../types';
import { cosRestorePath, describeExport, exportRepoOf } from '../../utils/backup-cos';
import { exportStateText } from '../../utils/backup-helpers';
import { columnInfo } from '../../utils/column-help';
import COSContents from './COSContents';
import ExportSetupGuide from './ExportSetupGuide';

const EXPORT_COLOR = { ok: 'green', warning: 'orange', danger: 'red' } as const;
const NOT_EXPORTING = 'This policy does not export to COS, so there is nothing to list.';

/** The COS tab of a policy: its export settings, the export state of each volume, and the list of what is in the bucket. */
const BackupCOSTab: React.FC<{ policy: BackupPolicy }> = ({ policy }) => {
  const history = useHistory();
  const name = policy.metadata?.name || '';
  const ex = policy.spec?.export;
  const repo = exportRepoOf(policy);
  const volumes = policy.status?.volumes || [];

  return (
    <Stack hasGutter style={{ marginTop: 16 }}>
      {!ex && (
        <StackItem>
          <Alert variant="info" isInline title="This policy does not export to COS">
            Backups stay in the snapshot pool only. An export also sends them, encrypted, to an IBM Cloud Object Storage bucket, so
            they survive the loss of the snapshot pool.
          </Alert>
        </StackItem>
      )}
      {ex && (
        <StackItem>
          <Card>
            <CardTitle>Export settings</CardTitle>
            <CardBody>
              <DescriptionList isHorizontal>
                {describeExport(ex).map((row) => (
                  <DescriptionListGroup key={row.label}>
                    <DescriptionListTerm>{row.label}</DescriptionListTerm>
                    <DescriptionListDescription>{row.value}</DescriptionListDescription>
                  </DescriptionListGroup>
                ))}
              </DescriptionList>
              <p style={{ marginTop: 12 }}>
                The console shows the names of the Secrets and never shows the contents of a Secret.
              </p>
            </CardBody>
          </Card>
        </StackItem>
      )}
      {ex && (
        <StackItem>
          <Card>
            <CardTitle>Export state of each volume</CardTitle>
            <CardBody>
              {volumes.length === 0 ? (
                'No volumes are backed up yet.'
              ) : (
                <Table aria-label="Export state of each volume" variant="compact">
                  <Thead>
                    <Tr>
                      <Th>Claim</Th>
                      <Th info={columnInfo('backupVolumeExport')}>Export</Th>
                    </Tr>
                  </Thead>
                  <Tbody>
                    {volumes.map((v) => {
                      const state = exportStateText(v, policy);
                      return (
                        <Tr key={v.subVolumeName || `${v.pvcNamespace}/${v.pvcName}`}>
                          <Td dataLabel="Claim">{`${v.pvcNamespace}/${v.pvcName}`}</Td>
                          <Td dataLabel="Export">
                            <Label color={EXPORT_COLOR[state.severity as 'ok' | 'warning' | 'danger']} isCompact>
                              {state.text}
                            </Label>
                          </Td>
                        </Tr>
                      );
                    })}
                  </Tbody>
                </Table>
              )}
            </CardBody>
          </Card>
        </StackItem>
      )}
      <StackItem>
        <ExportSetupGuide policy={policy} />
      </StackItem>
      <StackItem>
        <Card>
          <CardTitle>What is in the bucket</CardTitle>
          <CardBody>
            <COSContents
              repo={repo}
              unavailable={repo ? undefined : NOT_EXPORTING}
              onPick={(volume, moment) => history.push(cosRestorePath({ policy: name, volume, moment }))}
            />
            <p style={{ marginTop: 16 }}>
              <Link to={cosRestorePath({ policy: name })}>Restore from COS</Link>
            </p>
          </CardBody>
        </Card>
      </StackItem>
    </Stack>
  );
};

export default BackupCOSTab;
