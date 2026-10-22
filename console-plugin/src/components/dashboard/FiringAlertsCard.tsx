import React from 'react';
import { Alert, Card, CardBody, CardTitle, Label, Spinner } from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { FiringAlert } from '../../utils/firing-alerts';

interface Props {
  loading: boolean;
  error: string | null;
  rulesLoaded: boolean;
  firing: FiringAlert[];
}

const severityColor = (severity: string): 'red' | 'orange' | 'grey' =>
  severity === 'critical' ? 'red' : severity === 'warning' ? 'orange' : 'grey';

/** Alerts of the driver that Prometheus reports as firing. */
const FiringAlertsCard: React.FC<Props> = ({ loading, error, rulesLoaded, firing }) => (
  <Card isFullHeight>
    <CardTitle>Firing Alerts</CardTitle>
    <CardBody>
      {loading ? (
        <Spinner size="md" />
      ) : error ? (
        <Alert variant="warning" isInline isPlain title={`Cannot read alerts from Prometheus: ${error}`} />
      ) : !rulesLoaded ? (
        <Alert
          variant="info"
          isInline
          isPlain
          title="The cluster Prometheus does not evaluate the driver's alert rules, so this card cannot show alerts. Check that the PrometheusRule from the chart is picked up by your monitoring stack."
        />
      ) : firing.length === 0 ? (
        <Label color="green">No alerts firing</Label>
      ) : (
        <Table variant="compact" aria-label="Firing alerts">
          <Thead>
            <Tr>
              <Th>Alert</Th>
              <Th>Severity</Th>
              <Th>Where</Th>
            </Tr>
          </Thead>
          <Tbody>
            {firing.map((a) => (
              <Tr key={`${a.name}/${a.pool ?? ''}/${a.share ?? ''}`}>
                <Td>{a.name}</Td>
                <Td>
                  <Label color={severityColor(a.severity)}>{a.severity}</Label>
                </Td>
                <Td>{[a.pool, a.share].filter(Boolean).join(' / ') || '-'}</Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      )}
    </CardBody>
  </Card>
);

export default FiringAlertsCard;
