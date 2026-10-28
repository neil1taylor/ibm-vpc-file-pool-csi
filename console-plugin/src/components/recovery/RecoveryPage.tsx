import React, { useCallback, useEffect, useRef, useState } from 'react';
import { k8sCreate, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import {
  Alert,
  Bullseye,
  Button,
  Card,
  CardBody,
  CardTitle,
  FormGroup,
  FormSelect,
  FormSelectOption,
  Label,
  PageSection,
  Spinner,
  Split,
  SplitItem,
  Stack,
  StackItem,
  Title,
} from '@patternfly/react-core';
import { DeploymentModel, FailoverModel, FileSharePoolModel, ReplicationPolicyModel } from '../../models';
import { API_GROUP, DRIVER_NAMESPACE } from '../../constants';
import { Failover, FileSharePool, ReplicationPolicy } from '../../types';
import {
  Staleness,
  buildFailover,
  copyStaleness,
  describeFailoverState,
  describePlanRows,
  describeVMDetail,
  failoverSource,
  findPlan,
  isBusyMessage,
  needsNewPlan,
  planRemainingMs,
  receiverClaims,
  scheduleOfVM,
} from '../../utils/failover-helpers';
import { errorMessage } from '../../utils/k8s-objects';
import { formatDuration } from '../../utils/resource-helpers';
import FailoverConfirmModal from './FailoverConfirmModal';
import FailoverProgress from './FailoverProgress';
import ForceOption from './ForceOption';
import RecoveryVMDetail from './RecoveryVMDetail';
import RecoveryVMTable from './RecoveryVMTable';

const gvk = (model: { apiGroup?: string; apiVersion: string; kind: string }) => ({
  group: model.apiGroup,
  version: model.apiVersion,
  kind: model.kind,
});

const useNow = (): number => {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  return now;
};

const minutes = (ms: number) => Math.max(1, Math.ceil(ms / 60000));

/** The moment the operator chose, and the Plan that must show it before the confirm dialog opens. */
interface Choice {
  vm: string;
  moment: string;
  planName: string;
}

/**
 * Recovery on the destination (DR) cluster. The page makes a Failover with action Plan, shows the VMs it found
 * and the moments each can show, and after a confirm step makes a Failover with action Execute. The Plan and the
 * Execute run as a Job on this cluster (the same code as `kubectl failover`). The page uses the rights of the
 * signed-in user, and the Failover resource is cluster scoped, so it is for cluster administrators.
 */
const RecoveryPage: React.FC = () => {
  const now = useNow();
  const [canCreate, accessLoading] = useAccessReview({ group: API_GROUP, resource: 'failovers', verb: 'create' });
  const allowed = canCreate === true;

  const [failoverList, failoversLoaded] = useK8sWatchResource<Failover[]>(
    allowed ? { groupVersionKind: gvk(FailoverModel), isList: true } : null,
  );
  const [poolList] = useK8sWatchResource<FileSharePool[]>(
    allowed ? { groupVersionKind: gvk(FileSharePoolModel), isList: true } : null,
  );
  const [policyList] = useK8sWatchResource<ReplicationPolicy[]>(
    allowed ? { groupVersionKind: gvk(ReplicationPolicyModel), isList: true } : null,
  );
  const [deploymentList, deploymentsLoaded] = useK8sWatchResource<Array<Record<string, unknown>>>(
    allowed
      ? {
          groupVersionKind: gvk(DeploymentModel),
          namespace: DRIVER_NAMESPACE,
          isList: true,
          selector: { matchLabels: { 'app.kubernetes.io/component': 'replication-receiver' } },
        }
      : null,
  );

  const [chosenSource, setChosenSource] = useState('');
  const [activePlanName, setActivePlanName] = useState('');
  const [selectedVM, setSelectedVM] = useState('');
  const [choice, setChoice] = useState<Choice | undefined>();
  const [executeName, setExecuteName] = useState('');
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState('');
  const [dialogError, setDialogError] = useState('');
  const autoPlanned = useRef(false);

  const failovers = failoverList || [];
  const claims = receiverClaims((deploymentList || []) as never);
  const needsSourceChoice = claims.length > 1 && !chosenSource;
  const source = claims.length === 1 ? claims[0] : claims.length > 1 ? chosenSource : '';

  const plan = activePlanName
    ? failovers.find((f) => f.metadata?.name === activePlanName)
    : findPlan(failovers, source);
  const execute = executeName ? failovers.find((f) => f.metadata?.name === executeName) : undefined;
  const executeState = execute ? describeFailoverState(execute, now) : undefined;

  const createPlan = useCallback(
    async (opts: { rewindTo?: string; force?: boolean }): Promise<string> => {
      setError('');
      setCreating(true);
      try {
        const request = buildFailover({ action: 'Plan', source, ...opts }, Date.now());
        const made = await k8sCreate<Failover>({ model: FailoverModel, data: request });
        const name = made?.metadata?.name || request.metadata!.name!;
        setActivePlanName(name);
        return name;
      } catch (err: unknown) {
        setError(errorMessage(err, 'Failed to make the plan'));
        return '';
      } finally {
        setCreating(false);
      }
    },
    [source],
  );

  // A first visit with no plan makes one. Later visits show the newest plan until Refresh.
  const ready = allowed && failoversLoaded && deploymentsLoaded && !needsSourceChoice;
  useEffect(() => {
    if (!ready || plan || activePlanName || autoPlanned.current) return;
    autoPlanned.current = true;
    void createPlan({});
  }, [ready, plan, activePlanName, createPlan]);

  const chooseSource = (value: string) => {
    setChosenSource(value);
    setActivePlanName('');
    setSelectedVM('');
    setChoice(undefined);
    autoPlanned.current = false;
  };

  if (accessLoading) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }
  if (!allowed) {
    return (
      <PageSection>
        <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
          Recovery
        </Title>
        <Alert variant="warning" isInline title="You are not allowed to fail over">
          <p>
            Recovery makes Failover objects, which belong to the whole cluster. Only a cluster administrator can use
            this page.
          </p>
          <p>
            The role you need is the failover-admin cluster role of the driver (its name ends in -failover-admin).
            The driver does not give it to anyone. Ask an administrator to bind it to your account.
          </p>
        </Alert>
      </PageSection>
    );
  }

  const planState = plan ? describeFailoverState(plan, now) : undefined;
  const planWorking = !!activePlanName && !plan ? true : !!planState?.working;
  const planned = !!plan && plan.status?.phase === 'Planned';
  const remaining = plan ? planRemainingMs(plan, now) : 0;
  const expired = planned && remaining === 0;
  const rows = describePlanRows(plan, now);

  const staleness: Record<string, Staleness> = {};
  for (const vm of plan?.status?.vms || []) {
    staleness[vm.name] = copyStaleness(vm.moment, scheduleOfVM(plan!, vm, policyList || []), now);
  }

  const detail = plan && selectedVM ? describeVMDetail(plan, selectedVM, now) : undefined;
  const executeRunning = !!execute && !(executeState?.failed);
  const forcedPlan = !!plan?.spec?.force;

  const refresh = () => {
    setSelectedVM('');
    setChoice(undefined);
    setExecuteName('');
    void createPlan({ force: forcedPlan });
  };

  const planAgainWithForce = () => {
    setExecuteName('');
    setChoice(undefined);
    void createPlan({ force: true, rewindTo: plan?.spec?.rewindTo });
  };

  const startFailOver = async (moment: string) => {
    if (!plan || !selectedVM) return;
    setDialogError('');
    if (!needsNewPlan(plan, selectedVM, moment)) {
      setChoice({ vm: selectedVM, moment, planName: plan.metadata!.name! });
      return;
    }
    const name = await createPlan({ rewindTo: moment, force: forcedPlan });
    if (name) setChoice({ vm: selectedVM, moment, planName: name });
  };

  const confirm = async (drPool: string) => {
    if (!plan) return;
    setDialogError('');
    setCreating(true);
    try {
      const request = buildFailover(
        {
          action: 'Execute',
          source: failoverSource(plan),
          rewindTo: plan.spec.rewindTo,
          force: plan.spec.force,
          drPool,
        },
        Date.now(),
      );
      const made = await k8sCreate<Failover>({ model: FailoverModel, data: request });
      setExecuteName(made?.metadata?.name || request.metadata!.name!);
      setChoice(undefined);
    } catch (err: unknown) {
      setDialogError(errorMessage(err, 'Failed to start the failover'));
    } finally {
      setCreating(false);
    }
  };

  const choicePlanReady = !!choice && !!plan && plan.metadata?.name === choice.planName && planned;
  const choiceMismatch = choicePlanReady && needsNewPlan(plan!, choice!.vm, choice!.moment);
  const dialogOpen = choicePlanReady && !choiceMismatch && !expired;

  return (
    <PageSection variant="default">
      <Stack hasGutter>
        <StackItem>
          <Split hasGutter>
            <SplitItem isFilled>
              <Title headingLevel="h1">Recovery</Title>
              <p>
                Fail over the VMs on this cluster to the copy that replication made. The page reads the copy with a
                plan first, and nothing changes until you confirm.
              </p>
            </SplitItem>
            <SplitItem>
              <Button variant="secondary" onClick={refresh} isDisabled={creating || planWorking || needsSourceChoice}>
                Refresh
              </Button>
            </SplitItem>
          </Split>
        </StackItem>

        {claims.length > 1 && (
          <StackItem>
            <FormGroup label="Source" fieldId="failover-source">
              <FormSelect id="failover-source" value={chosenSource} onChange={(_e, v) => chooseSource(v)}>
                <FormSelectOption value="" label="Choose the copy to read" />
                {claims.map((c) => (
                  <FormSelectOption key={c} value={c} label={c} />
                ))}
              </FormSelect>
            </FormGroup>
          </StackItem>
        )}

        {error && (
          <StackItem>
            <Alert variant="danger" isInline title="The request was refused">
              {error}
            </Alert>
          </StackItem>
        )}

        {execute && (
          <StackItem>
            <FailoverProgress
              failover={execute}
              now={now}
              isBusy={creating}
              onPlanWithForce={planAgainWithForce}
            />
          </StackItem>
        )}

        {choice && choicePlanReady && choiceMismatch && (
          <StackItem>
            <Alert variant="danger" isInline title="The plan does not show the moment you chose">
              The copy changed since the page listed its moments. Choose a moment again.
            </Alert>
          </StackItem>
        )}

        {planWorking && (
          <StackItem>
            <Alert
              variant="info"
              isInline
              customIcon={<Spinner size="md" />}
              title={planState?.title || 'Still working: the plan is being made'}
            >
              <p>{planState?.detail || 'The driver is reading the copy.'}</p>
              {planState?.elapsed && <p>{`Running for ${planState.elapsed}.`}</p>}
            </Alert>
          </StackItem>
        )}

        {planState?.failed && plan && (
          <StackItem>
            <Alert variant="danger" isInline title="The plan failed">
              <p>{planState.message || 'The driver gave no reason.'}</p>
              {isBusyMessage(planState.message) && (
                <ForceOption onPlanWithForce={planAgainWithForce} isBusy={creating} />
              )}
            </Alert>
          </StackItem>
        )}

        {needsSourceChoice && (
          <StackItem>
            <Alert variant="info" isInline title="Choose the copy to read">
              This cluster has more than one replication receiver. Choose the copy to plan a failover from.
            </Alert>
          </StackItem>
        )}

        {planned && plan && (
          <StackItem>
            <Alert
              variant={expired ? 'warning' : 'info'}
              isInline
              title={
                expired
                  ? 'This plan is too old to fail over from. Click Refresh.'
                  : `Plan made from ${failoverSource(plan) || 'the default copy'}. It stays valid for ${minutes(remaining)} more ${minutes(remaining) === 1 ? 'minute' : 'minutes'}.`
              }
            >
              {plan.status?.rpo && <p>{`The oldest VM data is ${formatDuration(plan.status.rpo)} old.`}</p>}
              {plan.status?.momentsTruncated && (
                <p>The list of moments was cut to fit. The newest moments are shown.</p>
              )}
              {forcedPlan && <Label color="orange">Planned with force</Label>}
            </Alert>
          </StackItem>
        )}

        {planned && plan && !detail && (
          <StackItem>
            <Card>
              <CardTitle>Running VMs found on the copy</CardTitle>
              <CardBody>
                {rows.length === 0 ? (
                  <p>The copy holds no running VM that can be failed over.</p>
                ) : (
                  <RecoveryVMTable rows={rows} staleness={staleness} onSelect={setSelectedVM} />
                )}
              </CardBody>
            </Card>
          </StackItem>
        )}

        {planned && plan && detail && (
          <StackItem>
            <RecoveryVMDetail
              key={`${plan.metadata?.name}/${detail.name}`}
              detail={detail}
              onBack={() => setSelectedVM('')}
              onFailOver={startFailOver}
              isDisabled={expired || creating || executeRunning}
              disabledReason={
                expired
                  ? 'The plan is too old. Click Refresh.'
                  : executeRunning
                    ? 'A failover was started on this page.'
                    : undefined
              }
            />
          </StackItem>
        )}
      </Stack>

      {dialogOpen && plan && choice && (
        <FailoverConfirmModal
          plan={plan}
          vmName={choice.vm}
          pools={(poolList || []).map((p) => p.metadata?.name || '').filter(Boolean)}
          creating={creating}
          error={dialogError}
          onConfirm={confirm}
          onClose={() => setChoice(undefined)}
        />
      )}
    </PageSection>
  );
};

export default RecoveryPage;
