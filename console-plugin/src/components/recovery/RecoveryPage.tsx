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
  scopeOfPlan,
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

/**
 * What the operator chose, and the Plan that must show it before the confirm dialog opens. For one VM the Plan
 * is made for that VM only (spec.vms). An empty vm means every VM on the copy: the Plan on screen is used.
 */
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
  const [lastRun, setLastRun] = useState<(Choice & { rewindTo?: string }) | undefined>();
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
  const choicePlan = choice ? failovers.find((f) => f.metadata?.name === choice.planName) : undefined;
  const execute = executeName ? failovers.find((f) => f.metadata?.name === executeName) : undefined;
  const executeState = execute ? describeFailoverState(execute, now) : undefined;

  const createPlan = useCallback(
    async (opts: { rewindTo?: string; force?: boolean; vms?: string[] }): Promise<string> => {
      setError('');
      setCreating(true);
      try {
        const request = buildFailover({ action: 'Plan', source, ...opts }, Date.now());
        const made = await k8sCreate<Failover>({ model: FailoverModel, data: request });
        const name = made?.metadata?.name || request.metadata!.name!;
        // The table keeps showing the plan for all VMs; a plan for chosen VMs only feeds the confirm dialog.
        if (!opts.vms?.length) setActivePlanName(name);
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
    setLastRun(undefined);
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

  /** Plan the chosen VM again with force, and open the dialog for the new plan when it is ready. */
  const planVMAgainWithForce = async (run: Choice & { rewindTo?: string }) => {
    setExecuteName('');
    setDialogError('');
    const name = await createPlan({ vms: [run.vm], rewindTo: run.rewindTo, force: true });
    if (name) setChoice({ vm: run.vm, moment: run.moment, planName: name });
  };

  const forceAfterFailedExecute = () => {
    if (lastRun?.vm) void planVMAgainWithForce(lastRun);
    else planAgainWithForce();
  };

  // Failing over one VM makes a new Plan for that VM only. The Execute must match its VM list.
  const startFailOver = async (moment: string) => {
    if (!plan || !selectedVM) return;
    setDialogError('');
    setChoice(undefined);
    const rewindTo = needsNewPlan(plan, selectedVM, moment) ? moment : undefined;
    const name = await createPlan({ vms: [selectedVM], rewindTo, force: forcedPlan });
    if (name) setChoice({ vm: selectedVM, moment, planName: name });
  };

  // The explicit option for every VM on the copy uses the plan on screen, which covers all of them.
  const startFailOverAll = () => {
    if (!plan) return;
    setDialogError('');
    setChoice({ vm: '', moment: '', planName: plan.metadata!.name! });
  };

  const confirm = async (drPool: string) => {
    if (!choicePlan || !choice) return;
    setDialogError('');
    setCreating(true);
    try {
      const request = buildFailover(
        {
          action: 'Execute',
          source: failoverSource(choicePlan),
          rewindTo: choicePlan.spec.rewindTo,
          force: choicePlan.spec.force,
          vms: scopeOfPlan(choicePlan),
          drPool,
        },
        Date.now(),
      );
      const made = await k8sCreate<Failover>({ model: FailoverModel, data: request });
      setExecuteName(made?.metadata?.name || request.metadata!.name!);
      setLastRun({ ...choice, rewindTo: choicePlan.spec.rewindTo });
      setChoice(undefined);
    } catch (err: unknown) {
      setDialogError(errorMessage(err, 'Failed to start the failover'));
    } finally {
      setCreating(false);
    }
  };

  const choiceState = choicePlan ? describeFailoverState(choicePlan, now) : undefined;
  const choiceWorking = !!choice && choice.vm !== '' && (!choicePlan || !!choiceState?.working);
  const choicePlanReady = !!choice && !!choicePlan && choicePlan.status?.phase === 'Planned';
  const choiceExpired = choicePlanReady && planRemainingMs(choicePlan!, now) === 0;
  const choiceMismatch = choicePlanReady && choice!.vm !== '' && needsNewPlan(choicePlan!, choice!.vm, choice!.moment);
  const dialogOpen = choicePlanReady && !choiceMismatch && !choiceExpired;

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
              onPlanWithForce={forceAfterFailedExecute}
            />
          </StackItem>
        )}

        {choiceWorking && (
          <StackItem>
            <Alert
              variant="info"
              isInline
              customIcon={<Spinner size="md" />}
              title={`Still working: the plan for ${choice!.vm} is being made`}
            >
              <p>The driver is reading the disks of this VM. The confirm dialog opens when the plan is ready.</p>
              {choiceState?.elapsed && <p>{`Running for ${choiceState.elapsed}.`}</p>}
            </Alert>
          </StackItem>
        )}

        {choice && choicePlan && choiceState?.failed && (
          <StackItem>
            <Alert variant="danger" isInline title={`The plan for ${choice.vm || 'all VMs'} failed`}>
              <p>{choiceState.message || 'The driver gave no reason.'}</p>
              {isBusyMessage(choiceState.message) && choice.vm !== '' && (
                <ForceOption
                  onPlanWithForce={() => void planVMAgainWithForce({ ...choice, rewindTo: choicePlan.spec.rewindTo })}
                  isBusy={creating}
                />
              )}
            </Alert>
          </StackItem>
        )}

        {choice && choicePlanReady && choiceExpired && (
          <StackItem>
            <Alert variant="warning" isInline title="This plan is too old to fail over from">
              Choose the failover again to make a new plan.
            </Alert>
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
                  <>
                    <RecoveryVMTable rows={rows} staleness={staleness} onSelect={setSelectedVM} />
                    <p style={{ marginTop: 12 }}>
                      Choose a VM to fail over that VM only. To fail over every VM on the copy at once, use the
                      button below.
                    </p>
                    <Button
                      variant="secondary"
                      onClick={startFailOverAll}
                      isDisabled={expired || creating || executeRunning}
                    >
                      Fail over all VMs
                    </Button>
                  </>
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

      {dialogOpen && choicePlan && choice && (
        <FailoverConfirmModal
          plan={choicePlan}
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
