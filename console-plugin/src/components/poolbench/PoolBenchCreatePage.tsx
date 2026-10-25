import React, { useMemo, useState } from 'react';
import { k8sCreate, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { useHistory } from 'react-router-dom';
import {
  ActionGroup,
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  Checkbox,
  FormHelperText,
  Grid,
  GridItem,
  HelperText,
  HelperTextItem,
  Label,
  PageSection,
  Radio,
  Stack,
  StackItem,
  TextInput,
  Title,
} from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { DataSourceModel, NodeModel, PoolBenchModel } from '../../models';
import { API_GROUP, ROUTES } from '../../constants';
import FieldPopover from '../common/FieldPopover';
import {
  NodeLike,
  PRESETS,
  PoolBenchForm,
  defaultForm,
  diskNeedGB,
  dp2Range,
  memoryNeedGiB,
  nPlusOneMemoryGiB,
  runMinutes,
  stepDemand,
  toPoolBenches,
  validateForm,
} from '../../utils/poolbench-form';
import { errorMessage } from '../../utils/k8s-objects';
import { POOLBENCH_INTRO, POOLBENCH_SHARED_NODES } from './poolbench-texts';

const IMAGES_NS = 'openshift-virtualization-os-images';
const LINUX_IMAGE = 'centos-stream9';
const WINDOWS_IMAGE = 'win2k22-perf';

const gvk = (m: typeof PoolBenchModel) => ({ group: m.apiGroup || '', version: m.apiVersion, kind: m.kind });

const Helper: React.FC<{ error?: string; children?: React.ReactNode }> = ({ error, children }) => (
  <FormHelperText>
    <HelperText>
      {error && <HelperTextItem variant="error">{error}</HelperTextItem>}
      {children && <HelperTextItem>{children}</HelperTextItem>}
    </HelperText>
  </FormHelperText>
);

const hours = (minutes: number): string => `about ${Math.floor(minutes / 60)} h ${minutes % 60} min`;

interface NumberFieldProps {
  id: string;
  label: string;
  help: string;
  value: number;
  error?: string;
  hint?: React.ReactNode;
  onChange: (v: number) => void;
}

const NumberField: React.FC<NumberFieldProps> = ({ id, label, help, value, error, hint, onChange }) => (
  <FieldPopover fieldId={id} label={label} description={help}>
    <TextInput
      id={id}
      aria-label={label}
      type="number"
      value={Number.isFinite(value) ? value : ''}
      onChange={(_e, v) => onChange(Number(v))}
      validated={error ? 'error' : 'default'}
    />
    <Helper error={error}>{hint}</Helper>
  </FieldPopover>
);

interface Check {
  label: string;
  ok: boolean;
  text: string;
}

/** The new-run form: presets, every setting with help, live numbers, and the checks before Run. */
const PoolBenchCreatePage: React.FC = () => {
  const history = useHistory();
  const [form, setForm] = useState<PoolBenchForm>(defaultForm());
  const [preset, setPreset] = useState('tier');
  const [error, setError] = useState('');
  const [saving, setSaving] = useState(false);
  const [canCreate] = useAccessReview({ group: API_GROUP, resource: 'poolbenches', verb: 'create' });
  const [nodes] = useK8sWatchResource<NodeLike[]>({ groupVersionKind: gvk(NodeModel), isList: true });
  const [images] = useK8sWatchResource<{ metadata?: { name?: string } }[]>({
    groupVersionKind: gvk(DataSourceModel),
    namespace: IMAGES_NS,
    isList: true,
  });

  const set = <K extends keyof PoolBenchForm>(key: K, value: PoolBenchForm[K]) => setForm((f) => ({ ...f, [key]: value }));
  const errors = validateForm(form);
  const range = dp2Range(form.shareSizeGB);
  const steps = stepDemand(form);
  const runs = form.tiers.length || 1;

  const checks = useMemo<Check[]>(() => {
    const have = new Set((images || []).map((i) => i.metadata?.name));
    const out: Check[] = [
      { label: 'Linux image', ok: have.has(LINUX_IMAGE), text: `${IMAGES_NS}/${LINUX_IMAGE} ${have.has(LINUX_IMAGE) ? 'exists' : 'is missing'}` },
    ];
    if (form.windowsEvery > 0) {
      out.push({
        label: 'Windows image',
        ok: have.has(WINDOWS_IMAGE),
        text: `${IMAGES_NS}/${WINDOWS_IMAGE} ${have.has(WINDOWS_IMAGE) ? 'exists' : 'is missing: prepare it with test/perf/prepare-windows-image.sh'}`,
      });
    }
    const need = memoryNeedGiB(form);
    const free = nPlusOneMemoryGiB(nodes || []);
    out.push({
      label: 'Memory with one node down',
      ok: need <= free,
      text: `${need} GiB needed, ${free} GiB on the nodes with one node down (the service checks again with what other pods use)`,
    });
    out.push({
      label: 'Disks on the share',
      ok: diskNeedGB(form) <= form.shareSizeGB,
      text: `${diskNeedGB(form)} GB of disks on a ${form.shareSizeGB} GB share`,
    });
    return out;
  }, [form, images, nodes]);

  const blocked = Object.keys(errors).length > 0 || checks.some((c) => !c.ok) || !canCreate;

  const start = async () => {
    setError('');
    setSaving(true);
    try {
      const objects = toPoolBenches(form);
      for (const data of objects) {
        await k8sCreate({ model: PoolBenchModel, data });
      }
      history.push(ROUTES.POOLBENCH_DETAIL.replace(':name', objects[0].metadata?.name || ''));
    } catch (err) {
      setError(errorMessage(err, 'The run could not be made'));
      setSaving(false);
    }
  };

  return (
    <PageSection>
      <Title headingLevel="h1" style={{ marginBottom: 8 }}>
        New PoolBench run
      </Title>
      <p style={{ marginBottom: 16 }}>{POOLBENCH_INTRO}</p>
      <Stack hasGutter>
        {error && (
          <StackItem>
            <Alert variant="danger" isInline title="The run could not be made">
              {error}
            </Alert>
          </StackItem>
        )}
        {!canCreate && (
          <StackItem>
            <Alert variant="warning" isInline title="Starting a run needs the right to create PoolBench objects">
              Only a cluster administrator can start a run, because a run makes a namespace, a pool and VMs.
            </Alert>
          </StackItem>
        )}

        <StackItem>
          <Card>
            <CardTitle>What to run</CardTitle>
            <CardBody>
              {PRESETS.map((p) => (
                <Radio
                  key={p.id}
                  id={`pb-preset-${p.id}`}
                  name="pb-preset"
                  label={p.label}
                  description={p.description}
                  isChecked={preset === p.id}
                  onChange={() => {
                    setPreset(p.id);
                    setForm(p.apply(form));
                  }}
                />
              ))}
              <div style={{ marginTop: 16, maxWidth: 400 }}>
                <FieldPopover fieldId="pb-name" label="Name" description="The name of the run. With three tiers, each run gets the tier's IOPS added." isRequired>
                  <TextInput id="pb-name" value={form.name} onChange={(_e, v) => set('name', v)} validated={errors.name ? 'error' : 'default'} />
                  <Helper error={errors.name} />
                </FieldPopover>
              </div>
            </CardBody>
          </Card>
        </StackItem>

        <StackItem>
          <Card>
            <CardTitle>Share</CardTitle>
            <CardBody>
              <Grid hasGutter md={4}>
                <GridItem>
                  <NumberField id="pb-size" label="Share size (GB)" help="The size of the one share of the test pool. It must hold the disks of the largest step." value={form.shareSizeGB} error={errors.shareSizeGB} onChange={(v) => set('shareSizeGB', v)} />
                </GridItem>
                <GridItem>
                  {form.tiers.length ? (
                    <FieldPopover fieldId="pb-tiers" label="IOPS of each tier" description="One run for each share speed, one after another.">
                      <TextInput id="pb-tiers" value={form.tiers.join(', ')} onChange={(_e, v) => set('tiers', v.split(',').map((x) => Number(x.trim())).filter((x) => x > 0))} validated={errors.tiers ? 'error' : 'default'} />
                      <Helper error={errors.tiers}>{range && `IBM allows ${range.minIOPS} to ${range.maxIOPS} IOPS for ${form.shareSizeGB} GB`}</Helper>
                    </FieldPopover>
                  ) : (
                    <NumberField id="pb-iops" label="IOPS" help="The share's IOPS limit: the share speed under test." value={form.iops} error={errors.iops} hint={range && `IBM allows ${range.minIOPS} to ${range.maxIOPS} IOPS for ${form.shareSizeGB} GB`} onChange={(v) => set('iops', v)} />
                  )}
                </GridItem>
              </Grid>
            </CardBody>
          </Card>
        </StackItem>

        <StackItem>
          <Card>
            <CardTitle>VMs</CardTitle>
            <CardBody>
              <Grid hasGutter md={3}>
                <GridItem><NumberField id="pb-step" label="VMs per step" help="How many VMs are added at each step." value={form.stepVMs} error={errors.stepVMs} onChange={(v) => set('stepVMs', v)} /></GridItem>
                <GridItem><NumberField id="pb-max" label="Largest VM count" help="The run stops here, or at the knee." value={form.maxVMs} error={errors.maxVMs} onChange={(v) => set('maxVMs', v)} /></GridItem>
                <GridItem><NumberField id="pb-win" label="Windows: 1 VM in" help="Every Nth VM runs Windows Server 2022; 0 for Linux only." value={form.windowsEvery} onChange={(v) => set('windowsEvery', v)} /></GridItem>
                <GridItem />
                <GridItem><NumberField id="pb-lcpu" label="Linux vCPU" help="vCPUs of each Linux VM (CentOS Stream 9)." value={form.linuxCPU} onChange={(v) => set('linuxCPU', v)} /></GridItem>
                <GridItem><NumberField id="pb-lmem" label="Linux memory (GiB)" help="Memory of each Linux VM." value={form.linuxMemoryGiB} onChange={(v) => set('linuxMemoryGiB', v)} /></GridItem>
                <GridItem><NumberField id="pb-wcpu" label="Windows vCPU" help="vCPUs of each Windows VM." value={form.windowsCPU} onChange={(v) => set('windowsCPU', v)} /></GridItem>
                <GridItem><NumberField id="pb-wmem" label="Windows memory (GiB)" help="Memory of each Windows VM." value={form.windowsMemoryGiB} onChange={(v) => set('windowsMemoryGiB', v)} /></GridItem>
              </Grid>
            </CardBody>
          </Card>
        </StackItem>

        <StackItem>
          <Card>
            <CardTitle>Load of each VM</CardTitle>
            <CardBody>
              <Grid hasGutter md={3}>
                <GridItem><NumberField id="pb-steady" label="Steady IOPS" help="The rate between bursts." value={form.steadyIOPS} error={errors.steadyIOPS} onChange={(v) => set('steadyIOPS', v)} /></GridItem>
                <GridItem><NumberField id="pb-burst" label="Burst IOPS" help="The rate of a burst, which stands for a boot, a patch or a backup scan." value={form.burstIOPS} error={errors.burstIOPS} onChange={(v) => set('burstIOPS', v)} /></GridItem>
                <GridItem><NumberField id="pb-bsecs" label="Burst length (s)" help="How long a burst lasts." value={form.burstSeconds} error={errors.burstSeconds} onChange={(v) => set('burstSeconds', v)} /></GridItem>
                <GridItem><NumberField id="pb-period" label="Period (s)" help="Time from the start of one burst to the next." value={form.periodSeconds} onChange={(v) => set('periodSeconds', v)} /></GridItem>
                <GridItem><NumberField id="pb-read" label="Reads (%)" help="The share of reads; the rest are writes." value={form.readPercent} error={errors.readPercent} onChange={(v) => set('readPercent', v)} /></GridItem>
                <GridItem>
                  <FieldPopover fieldId="pb-mix" label="Block sizes" description="Each block size and its share of the I/O, in percent. IBM counts one operation for every 16 KiB of an I/O.">
                    <TextInput id="pb-mix" value={form.blockMix} onChange={(_e, v) => set('blockMix', v)} validated={errors.blockMix ? 'error' : 'default'} />
                    <Helper error={errors.blockMix} />
                  </FieldPopover>
                </GridItem>
                <GridItem><NumberField id="pb-area" label="Test area (GiB)" help="The part of each VM's load disk that the load reads and writes. It is written once before measuring." value={form.areaGiB} error={errors.areaGiB} onChange={(v) => set('areaGiB', v)} /></GridItem>
              </Grid>
            </CardBody>
          </Card>
        </StackItem>

        <StackItem>
          <Card>
            <CardTitle>Each step, and the knee</CardTitle>
            <CardBody>
              <Grid hasGutter md={3}>
                <GridItem><NumberField id="pb-settle" label="Settle (min)" help="The wait after the new VMs are ready. Making and filling new VMs slows the others for about 20 minutes." value={form.settleMinutes} error={errors.settleMinutes} onChange={(v) => set('settleMinutes', v)} /></GridItem>
                <GridItem><NumberField id="pb-measure" label="Measure (min)" help="How long each step is measured. At least one load period." value={form.measureMinutes} error={errors.measureMinutes} onChange={(v) => set('measureMinutes', v)} /></GridItem>
                <GridItem><NumberField id="pb-limit" label="Latency limit (ms)" help="A VM whose p95 latency is over this is slow." value={form.latencyLimitMs} error={errors.latencyLimitMs} onChange={(v) => set('latencyLimitMs', v)} /></GridItem>
                <GridItem><NumberField id="pb-slow" label="Slow VMs at the knee (%)" help="The step fails when more than this share of the VMs is slow." value={form.slowVMPercent} error={errors.slowVMPercent} onChange={(v) => set('slowVMPercent', v)} /></GridItem>
                <GridItem><NumberField id="pb-stall" label="Stall (s)" help="One I/O that takes longer than this is a stall, and any stall fails the step." value={form.stallSeconds} error={errors.stallSeconds} onChange={(v) => set('stallSeconds', v)} /></GridItem>
              </Grid>
              <Checkbox id="pb-stop" label="Stop at the knee" description="Without it, the run goes on to the largest VM count." isChecked={form.stopAtKnee} onChange={(_e, v) => set('stopAtKnee', v)} style={{ marginTop: 16 }} />
              <Checkbox id="pb-keep" label="Keep the VMs and the pool after the end" description="For a look after the run. They cost money until you clean up." isChecked={form.keepAfterEnd} onChange={(_e, v) => set('keepAfterEnd', v)} />
            </CardBody>
          </Card>
        </StackItem>

        <StackItem>
          <Card>
            <CardTitle>What the run will do</CardTitle>
            <CardBody>
              <p style={{ marginBottom: 8 }}>
                {runs === 1 ? 'One run' : `${runs} runs, one after another`}, each {hours(runMinutes(form))} if it reaches its last
                step. Each run makes a {form.shareSizeGB} GB share for its length and deletes it at the end.
              </p>
              <Table aria-label="Load per step" variant="compact" role="grid">
                <Thead>
                  <Tr>
                    <Th>VMs</Th>
                    <Th>Windows</Th>
                    <Th>Steady guest IOPS</Th>
                    <Th>IBM operations</Th>
                    <Th>Of the limit</Th>
                    <Th>If all burst at once</Th>
                  </Tr>
                </Thead>
                <Tbody>
                  {steps.map((s) => (
                    <Tr key={s.vms}>
                      <Td>{s.vms}</Td>
                      <Td>{s.windows}</Td>
                      <Td>{s.steadyIOPS}</Td>
                      <Td>{s.ibmOps}</Td>
                      <Td>{s.percentOfLimit}%</Td>
                      <Td>{s.burstIOPS}</Td>
                    </Tr>
                  ))}
                </Tbody>
              </Table>
            </CardBody>
          </Card>
        </StackItem>

        <StackItem>
          <Card data-testid="poolbench-checks">
            <CardTitle>Checks before the run</CardTitle>
            <CardBody>
              {checks.map((c) => (
                <div key={c.label} style={{ marginBottom: 4 }}>
                  <Label color={c.ok ? 'green' : 'red'}>{c.ok ? 'OK' : 'Blocks the run'}</Label> {c.label}: {c.text}
                </div>
              ))}
              <p style={{ marginTop: 8 }}>{POOLBENCH_SHARED_NODES}</p>
            </CardBody>
          </Card>
        </StackItem>

        <StackItem>
          <ActionGroup>
            <Button variant="primary" onClick={start} isDisabled={blocked || saving} isLoading={saving}>
              Start the run
            </Button>
            <Button variant="link" onClick={() => history.push(ROUTES.POOLBENCH)}>
              Cancel
            </Button>
          </ActionGroup>
        </StackItem>
      </Stack>
    </PageSection>
  );
};

export default PoolBenchCreatePage;
