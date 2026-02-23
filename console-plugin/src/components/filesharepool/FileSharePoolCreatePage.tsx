import React, { useState } from 'react';
import { k8sCreate, useK8sWatchResource, K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';
import { useHistory } from 'react-router-dom';
import {
  Wizard,
  WizardStep,
  PageSection,
  Title,
  Alert,
  FormHelperText,
  HelperText,
  HelperTextItem,
  TextInput,
  NumberInput,
  FormSelect,
  FormSelectOption,
  Switch,
  ActionGroup,
  Button,
  Stack,
  StackItem,
  TextArea,
  Card,
  CardHeader,
  CardTitle,
  CardBody,
  Grid,
  GridItem,
  Split,
  SplitItem,
  Label,
  DescriptionList,
  DescriptionListGroup,
  DescriptionListTerm,
  DescriptionListDescription,
} from '@patternfly/react-core';
import { FileSharePoolModel } from '../../models';
import { FileSharePool, FileSharePoolSpec, GoldenImageConfig } from '../../types';
import { ROUTES } from '../../constants';
import FieldPopover from '../common/FieldPopover';
import YAMLEditorFallback from '../common/YAMLEditorFallback';

// ── Preset configurations ──

interface Preset {
  id: string;
  label: string;
  description: string;
  values: Partial<FormState>;
}

const PRESETS: Preset[] = [
  {
    id: 'standard',
    label: 'Standard Workloads',
    description: 'Balanced settings for general Kubernetes workloads. 1 TB shares, spread allocation, auto-expand enabled.',
    values: {
      profile: 'dp2',
      shareSizeGB: 1000,
      maxShares: 10,
      initialShares: 1,
      autoExpand: true,
      expandThresholdPercent: 80,
      allocationStrategy: 'spread',
      defaultPermissions: '0755',
      defaultUID: '',
      defaultGID: '',
      encryptionInTransit: false,
      goldenImagesEnabled: false,
    },
  },
  {
    id: 'kubevirt',
    label: 'KubeVirt VMs',
    description: 'Optimized for virtual machine disk storage. UID 107 for QEMU, 0777 permissions, golden image syncer ready.',
    values: {
      profile: 'dp2',
      shareSizeGB: 2000,
      maxShares: 5,
      initialShares: 1,
      autoExpand: true,
      expandThresholdPercent: 75,
      allocationStrategy: 'spread',
      defaultPermissions: '0777',
      defaultUID: '107',
      defaultGID: '107',
      encryptionInTransit: false,
      goldenImagesEnabled: true,
      refreshInterval: '24h',
      converterImage: 'quay.io/centos/centos:stream9',
      pvcSizeGB: 15,
    },
  },
  {
    id: 'highiops',
    label: 'High-IOPS Database',
    description: 'Maximum performance for database workloads. Custom IOPS profile, binpack allocation to minimize share count.',
    values: {
      profile: 'custom',
      shareSizeGB: 4000,
      iops: 48000,
      maxShares: 5,
      initialShares: 2,
      autoExpand: false,
      expandThresholdPercent: 90,
      allocationStrategy: 'binpack',
      defaultPermissions: '0755',
      defaultUID: '',
      defaultGID: '',
      encryptionInTransit: true,
      goldenImagesEnabled: false,
    },
  },
];

interface FormState {
  name: string;
  zone: string;
  profile: string;
  shareSizeGB: number;
  iops: number;
  maxShares: number;
  initialShares: number;
  autoExpand: boolean;
  expandThresholdPercent: number;
  allocationStrategy: 'spread' | 'binpack';
  defaultPermissions: string;
  defaultUID: string;
  defaultGID: string;
  encryptionInTransit: boolean;
  resourceGroup: string;
  tags: string;
  mountOptions: string;
  goldenImagesEnabled: boolean;
  targetNamespaces: string;
  imageFilter: string;
  refreshInterval: string;
  converterImage: string;
  pvcSizeGB: number;
}

const initialFormState: FormState = {
  name: '',
  zone: '',
  profile: 'dp2',
  shareSizeGB: 1000,
  iops: 100,
  maxShares: 10,
  initialShares: 1,
  autoExpand: true,
  expandThresholdPercent: 80,
  allocationStrategy: 'spread',
  defaultPermissions: '0755',
  defaultUID: '',
  defaultGID: '',
  encryptionInTransit: false,
  resourceGroup: '',
  tags: '',
  mountOptions: '',
  goldenImagesEnabled: false,
  targetNamespaces: '',
  imageFilter: '',
  refreshInterval: '24h',
  converterImage: 'quay.io/centos/centos:stream9',
  pvcSizeGB: 15,
};

const parseLines = (text: string): string[] =>
  text
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l.length > 0);

const buildResource = (form: FormState): FileSharePool => {
  const spec: FileSharePoolSpec = {
    zone: form.zone,
    profile: form.profile,
    shareSizeGB: form.shareSizeGB,
    maxShares: form.maxShares,
    initialShares: form.initialShares,
    autoExpand: form.autoExpand,
    expandThresholdPercent: form.expandThresholdPercent,
    allocationStrategy: form.allocationStrategy,
    encryptionInTransit: form.encryptionInTransit,
    defaultPermissions: form.defaultPermissions,
  };

  if (form.profile === 'custom' && form.iops >= 100) {
    spec.iops = form.iops;
  }

  if (form.defaultUID !== '') {
    const uid = parseInt(form.defaultUID, 10);
    if (!isNaN(uid)) spec.defaultUID = uid;
  }

  if (form.defaultGID !== '') {
    const gid = parseInt(form.defaultGID, 10);
    if (!isNaN(gid)) spec.defaultGID = gid;
  }

  if (form.resourceGroup.trim()) {
    spec.resourceGroup = form.resourceGroup.trim();
  }

  const tags = parseLines(form.tags);
  if (tags.length > 0) spec.tags = tags;

  const mountOpts = parseLines(form.mountOptions);
  if (mountOpts.length > 0) spec.mountOptions = mountOpts;

  if (form.goldenImagesEnabled) {
    const goldenImages: GoldenImageConfig = {
      enabled: true,
      targetNamespaces: parseLines(form.targetNamespaces),
    };

    const filters = parseLines(form.imageFilter);
    if (filters.length > 0) goldenImages.imageFilter = filters;
    if (form.refreshInterval.trim()) goldenImages.refreshInterval = form.refreshInterval.trim();
    if (form.converterImage.trim()) goldenImages.converterImage = form.converterImage.trim();
    if (form.pvcSizeGB >= 5) goldenImages.pvcSizeGB = form.pvcSizeGB;

    spec.goldenImages = goldenImages;
  }

  return {
    apiVersion: 'storage.ibmcloud.io/v1alpha1',
    kind: 'FileSharePool',
    metadata: { name: form.name },
    spec,
  };
};

const toYAML = (obj: unknown, indent = 0): string => {
  const pad = '  '.repeat(indent);

  if (obj === null || obj === undefined) return 'null';

  if (typeof obj === 'string') {
    if (
      obj === '' ||
      obj.includes(':') ||
      obj.includes('#') ||
      obj.includes('\n') ||
      /^[{[]/.test(obj) ||
      /^(true|false|null|yes|no)$/i.test(obj) ||
      /^\d/.test(obj)
    ) {
      return JSON.stringify(obj);
    }
    return obj;
  }

  if (typeof obj === 'number' || typeof obj === 'boolean') return String(obj);

  if (Array.isArray(obj)) {
    if (obj.length === 0) return '[]';
    return obj
      .map((item) => {
        if (typeof item === 'object' && item !== null && !Array.isArray(item)) {
          const entries = Object.entries(item);
          if (entries.length === 0) return `${pad}- {}`;
          const [firstKey, firstVal] = entries[0];
          const firstLine = `${pad}- ${firstKey}: ${toYAML(firstVal, indent + 2)}`;
          const rest = entries
            .slice(1)
            .map(([k, v]) => `${pad}  ${k}: ${toYAML(v, indent + 2)}`)
            .join('\n');
          return rest ? `${firstLine}\n${rest}` : firstLine;
        }
        return `${pad}- ${toYAML(item, indent + 1)}`;
      })
      .join('\n');
  }

  if (typeof obj === 'object') {
    const entries = Object.entries(obj as Record<string, unknown>).filter(
      ([, v]) => v !== undefined,
    );
    if (entries.length === 0) return '{}';
    return entries
      .map(([key, value]) => {
        if (typeof value === 'object' && value !== null) {
          const nested = toYAML(value, indent + 1);
          if (nested.includes('\n') || (Array.isArray(value) && value.length > 0)) {
            return `${pad}${key}:\n${nested}`;
          }
          return `${pad}${key}: ${nested}`;
        }
        return `${pad}${key}: ${toYAML(value, indent + 1)}`;
      })
      .join('\n');
  }

  return String(obj);
};

const NAME_PATTERN = /^[a-z0-9-]+$/;
const ZONE_PATTERN = /^[a-z]{2}-[a-z]+-\d+$/;

/** Discover VPC zones from cluster worker node labels. */
function useNodeZones(): { zones: string[]; loaded: boolean } {
  const [nodes, loaded] = useK8sWatchResource<K8sResourceCommon[]>({
    groupVersionKind: { group: '', version: 'v1', kind: 'Node' },
    isList: true,
  });

  const zones = React.useMemo(() => {
    if (!loaded || !nodes) return [];
    const zoneSet = new Set<string>();
    for (const node of nodes) {
      const z = node.metadata?.labels?.['topology.kubernetes.io/zone'];
      if (z && ZONE_PATTERN.test(z)) zoneSet.add(z);
    }
    return Array.from(zoneSet).sort();
  }, [nodes, loaded]);

  return { zones, loaded };
}

const FileSharePoolCreatePage: React.FC = () => {
  const history = useHistory();
  const [form, setForm] = useState<FormState>(initialFormState);
  const [error, setError] = useState<string>('');
  const [creating, setCreating] = useState(false);
  const [selectedPreset, setSelectedPreset] = useState<string>('');
  const { zones: discoveredZones, loaded: zonesLoaded } = useNodeZones();

  const set = <K extends keyof FormState>(key: K, value: FormState[K]) =>
    setForm((prev) => ({ ...prev, [key]: value }));

  React.useEffect(() => {
    if (zonesLoaded && discoveredZones.length > 0 && form.zone === '') {
      set('zone', discoveredZones[0]);
    }
  }, [zonesLoaded, discoveredZones]);

  const applyPreset = (presetId: string) => {
    const preset = PRESETS.find((p) => p.id === presetId);
    if (preset) {
      setForm((prev) => ({ ...prev, ...preset.values }));
      setSelectedPreset(presetId);
    }
  };

  // Validation
  const isBasicValid =
    form.name.length > 0 &&
    NAME_PATTERN.test(form.name) &&
    form.zone.length > 0 &&
    ZONE_PATTERN.test(form.zone) &&
    form.shareSizeGB >= 10 &&
    form.shareSizeGB <= 32000 &&
    (form.profile !== 'custom' || form.iops >= 100);

  const isPoolSizingValid =
    form.maxShares >= 1 &&
    form.maxShares <= 100 &&
    form.initialShares >= 0 &&
    form.initialShares <= form.maxShares &&
    (!form.autoExpand ||
      (form.expandThresholdPercent >= 50 && form.expandThresholdPercent <= 95));

  const isGoldenImagesValid =
    !form.goldenImagesEnabled ||
    (parseLines(form.targetNamespaces).length > 0 && form.pvcSizeGB >= 5);

  const handleCreate = async () => {
    setError('');
    setCreating(true);
    try {
      const resource = buildResource(form);
      await k8sCreate({ model: FileSharePoolModel, data: resource });
      history.push(ROUTES.POOL_DETAIL.replace(':name', form.name));
    } catch (err: unknown) {
      const message =
        err instanceof Error
          ? err.message
          : typeof err === 'object' && err !== null && 'message' in err
            ? String((err as { message: unknown }).message)
            : 'Failed to create FileSharePool';
      setError(message);
    } finally {
      setCreating(false);
    }
  };

  // Capacity calculator
  const maxCapacity = form.shareSizeGB * form.maxShares;

  // ── Step content ──

  const basicStep = (
    <Stack hasGutter>
      {/* Preset selector */}
      <StackItem>
        <Card isCompact>
          <CardTitle>Quick Start Presets</CardTitle>
          <CardBody>
            <Grid hasGutter>
              {PRESETS.map((preset) => (
                <GridItem key={preset.id} sm={12} md={4}>
                  <Card
                    isSelectable
                    isSelected={selectedPreset === preset.id}
                    isCompact
                    isFullHeight
                  >
                    <CardHeader
                      selectableActions={{
                        selectableActionId: `preset-${preset.id}`,
                        selectableActionAriaLabel: preset.label,
                        name: 'preset-selector',
                        variant: 'single',
                        onChange: () => applyPreset(preset.id),
                      }}
                    >
                      <CardTitle>{preset.label}</CardTitle>
                    </CardHeader>
                    <CardBody>{preset.description}</CardBody>
                  </Card>
                </GridItem>
              ))}
            </Grid>
          </CardBody>
        </Card>
      </StackItem>

      <StackItem>
        <FieldPopover
          fieldId="fsp-name"
          label="Name"
          description="Unique identifier for this pool. Used in StorageClass parameters and SubVolume references. Lowercase letters, numbers, and hyphens only."
          isRequired
        >
          <TextInput
            id="fsp-name"
            value={form.name}
            onChange={(_event, value) => set('name', value)}
            isRequired
            validated={form.name.length === 0 || NAME_PATTERN.test(form.name) ? 'default' : 'error'}
            placeholder="my-pool"
          />
          <FormHelperText>
            <HelperText>
              <HelperTextItem>Lowercase letters, numbers, and hyphens only</HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FieldPopover>
      </StackItem>

      <StackItem>
        <FieldPopover
          fieldId="fsp-zone"
          label="Zone"
          description="The VPC availability zone where file shares will be created. Must match the zone of your worker nodes."
          isRequired
        >
          {discoveredZones.length > 0 ? (
            <FormSelect
              id="fsp-zone"
              value={form.zone}
              onChange={(_event, value) => set('zone', value)}
            >
              {discoveredZones.map((z) => (
                <FormSelectOption key={z} value={z} label={z} />
              ))}
            </FormSelect>
          ) : (
            <TextInput
              id="fsp-zone"
              value={form.zone}
              onChange={(_event, value) => set('zone', value)}
              isRequired
              validated={form.zone.length === 0 || ZONE_PATTERN.test(form.zone) ? 'default' : 'error'}
              placeholder={zonesLoaded ? 'us-south-1' : 'Loading zones...'}
              isDisabled={!zonesLoaded}
            />
          )}
          <FormHelperText>
            <HelperText>
              <HelperTextItem>
                {discoveredZones.length > 0
                  ? `${discoveredZones.length} zone${discoveredZones.length > 1 ? 's' : ''} detected from cluster nodes`
                  : 'VPC zone, e.g. us-south-1'}
              </HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FieldPopover>
      </StackItem>

      <StackItem>
        <FieldPopover
          fieldId="fsp-profile"
          label="Profile"
          description="VPC file share performance profile. dp2 provides 100 IOPS/GB with balanced performance for most workloads. Custom lets you specify exact IOPS."
          isRequired
        >
          <FormSelect
            id="fsp-profile"
            value={form.profile}
            onChange={(_event, value) => set('profile', value)}
          >
            <FormSelectOption value="dp2" label="dp2 - 100 IOPS/GB, balanced performance" />
            <FormSelectOption value="custom" label="custom - Specify exact IOPS" />
          </FormSelect>
        </FieldPopover>
      </StackItem>

      <StackItem>
        <FieldPopover
          fieldId="fsp-share-size"
          label="Share Size (GB)"
          description="Size of each individual VPC file share in the pool. All shares in the pool are the same size. Range: 10-32,000 GB. Larger shares reduce the number needed but take longer to provision (30-90s via VPC API)."
          isRequired
        >
          <NumberInput
            id="fsp-share-size"
            value={form.shareSizeGB}
            min={10}
            max={32000}
            onMinus={() => set('shareSizeGB', Math.max(10, form.shareSizeGB - 100))}
            onPlus={() => set('shareSizeGB', Math.min(32000, form.shareSizeGB + 100))}
            onChange={(event) => {
              const val = parseInt((event.target as HTMLInputElement).value, 10);
              if (!isNaN(val)) set('shareSizeGB', val);
            }}
          />
        </FieldPopover>
      </StackItem>

      {form.profile === 'custom' && (
        <StackItem>
          <FieldPopover
            fieldId="fsp-iops"
            label="IOPS"
            description="Maximum IOPS for each share. Higher IOPS increases cost. Minimum 100. For dp2, IOPS is automatically 100 per GB of share size."
            isRequired
          >
            <NumberInput
              id="fsp-iops"
              value={form.iops}
              min={100}
              onMinus={() => set('iops', Math.max(100, form.iops - 100))}
              onPlus={() => set('iops', form.iops + 100)}
              onChange={(event) => {
                const val = parseInt((event.target as HTMLInputElement).value, 10);
                if (!isNaN(val)) set('iops', val);
              }}
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>Minimum 100</HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>
      )}

      {/* Capacity calculator */}
      <StackItem>
        <Card isCompact isPlain>
          <CardTitle>Capacity Calculator</CardTitle>
          <CardBody>
            <Split hasGutter>
              <SplitItem>
                <strong>{form.shareSizeGB} GB</strong> x <strong>{form.maxShares}</strong> shares
              </SplitItem>
              <SplitItem>= </SplitItem>
              <SplitItem>
                <Label color="blue">{maxCapacity.toLocaleString()} GB max pool capacity</Label>
              </SplitItem>
            </Split>
          </CardBody>
        </Card>
      </StackItem>
    </Stack>
  );

  const poolSizingStep = (
    <Stack hasGutter>
      <StackItem>
        <FieldPopover
          fieldId="fsp-max-shares"
          label="Max Shares"
          description="Maximum number of VPC file shares the pool can create. Each share is a separate NFS export. More shares means more capacity but also more VPC resources. Range: 1-100."
          isRequired
        >
          <NumberInput
            id="fsp-max-shares"
            value={form.maxShares}
            min={1}
            max={100}
            onMinus={() => set('maxShares', Math.max(1, form.maxShares - 1))}
            onPlus={() => set('maxShares', Math.min(100, form.maxShares + 1))}
            onChange={(event) => {
              const val = parseInt((event.target as HTMLInputElement).value, 10);
              if (!isNaN(val)) set('maxShares', val);
            }}
          />
        </FieldPopover>
      </StackItem>

      <StackItem>
        <FieldPopover
          fieldId="fsp-initial-shares"
          label="Initial Shares"
          description="Number of shares to pre-provision when the pool is created. Pre-provisioning avoids the 30-90s VPC API delay on first PVC creation, but costs more upfront. Set to 0 for on-demand provisioning."
          isRequired
        >
          <NumberInput
            id="fsp-initial-shares"
            value={form.initialShares}
            min={0}
            max={form.maxShares}
            onMinus={() => set('initialShares', Math.max(0, form.initialShares - 1))}
            onPlus={() => set('initialShares', Math.min(form.maxShares, form.initialShares + 1))}
            onChange={(event) => {
              const val = parseInt((event.target as HTMLInputElement).value, 10);
              if (!isNaN(val)) set('initialShares', val);
            }}
          />
          <FormHelperText>
            <HelperText>
              <HelperTextItem>{`0 to ${form.maxShares}`}</HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FieldPopover>
      </StackItem>

      <StackItem>
        <FieldPopover
          fieldId="fsp-auto-expand"
          label="Auto-Expand"
          description="Automatically provision new file shares when pool utilization exceeds the threshold. Prevents PVC creation failures due to insufficient capacity."
        >
          <Switch
            id="fsp-auto-expand"
            label="Auto-expand pool when capacity is low"
            isChecked={form.autoExpand}
            onChange={(_event, checked) => set('autoExpand', checked)}
          />
        </FieldPopover>
      </StackItem>

      {form.autoExpand && (
        <StackItem>
          <FieldPopover
            fieldId="fsp-expand-threshold"
            label="Expand Threshold (%)"
            description="When pool utilization reaches this percentage, a new share is provisioned. Lower values trigger expansion earlier (more headroom, higher cost). Typical: 80%."
            isRequired
          >
            <NumberInput
              id="fsp-expand-threshold"
              value={form.expandThresholdPercent}
              min={50}
              max={95}
              onMinus={() => set('expandThresholdPercent', Math.max(50, form.expandThresholdPercent - 5))}
              onPlus={() => set('expandThresholdPercent', Math.min(95, form.expandThresholdPercent + 5))}
              onChange={(event) => {
                const val = parseInt((event.target as HTMLInputElement).value, 10);
                if (!isNaN(val)) set('expandThresholdPercent', val);
              }}
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>50 to 95</HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>
      )}

      <StackItem>
        <FieldPopover
          fieldId="fsp-strategy"
          label="Allocation Strategy"
          description="How new PVCs are placed across shares. Spread distributes evenly for fault isolation. Binpack fills shares sequentially to minimize the number of active shares."
          isRequired
        >
          <Grid hasGutter>
            <GridItem sm={12} md={6}>
              <Card
                isSelectable
                isSelected={form.allocationStrategy === 'spread'}
                isCompact
              >
                <CardHeader
                  selectableActions={{
                    selectableActionId: 'strategy-spread',
                    selectableActionAriaLabel: 'Spread allocation strategy',
                    name: 'allocation-strategy',
                    variant: 'single',
                    onChange: () => set('allocationStrategy', 'spread'),
                  }}
                >
                  <CardTitle>Spread</CardTitle>
                </CardHeader>
                <CardBody>
                  Distribute PVCs evenly across all shares. Best for fault isolation — if one share degrades, fewer PVCs are affected.
                </CardBody>
              </Card>
            </GridItem>
            <GridItem sm={12} md={6}>
              <Card
                isSelectable
                isSelected={form.allocationStrategy === 'binpack'}
                isCompact
              >
                <CardHeader
                  selectableActions={{
                    selectableActionId: 'strategy-binpack',
                    selectableActionAriaLabel: 'Binpack allocation strategy',
                    name: 'allocation-strategy',
                    variant: 'single',
                    onChange: () => set('allocationStrategy', 'binpack'),
                  }}
                >
                  <CardTitle>Binpack</CardTitle>
                </CardHeader>
                <CardBody>
                  Fill shares sequentially before using the next. Minimizes active share count and cost, but concentrates risk.
                </CardBody>
              </Card>
            </GridItem>
          </Grid>
        </FieldPopover>
      </StackItem>

      {/* Updated capacity calculator */}
      <StackItem>
        <Card isCompact isPlain>
          <CardTitle>Capacity Calculator</CardTitle>
          <CardBody>
            <Split hasGutter>
              <SplitItem>
                <strong>{form.shareSizeGB} GB</strong> x <strong>{form.maxShares}</strong> shares
              </SplitItem>
              <SplitItem>= </SplitItem>
              <SplitItem>
                <Label color="blue">{maxCapacity.toLocaleString()} GB max</Label>
              </SplitItem>
              {form.initialShares > 0 && (
                <SplitItem>
                  <Label color="grey">
                    {(form.shareSizeGB * form.initialShares).toLocaleString()} GB initial
                  </Label>
                </SplitItem>
              )}
            </Split>
          </CardBody>
        </Card>
      </StackItem>
    </Stack>
  );

  const permissionsStep = (
    <Stack hasGutter>
      <StackItem>
        <FieldPopover
          fieldId="fsp-permissions"
          label="Default Permissions"
          description="Unix file permissions for new subdirectories. Set to 0755 for standard workloads or 0777 for KubeVirt VMs (which need QEMU write access)."
        >
          <TextInput
            id="fsp-permissions"
            value={form.defaultPermissions}
            onChange={(_event, value) => set('defaultPermissions', value)}
            placeholder="0755"
          />
          <FormHelperText>
            <HelperText>
              <HelperTextItem>Octal, e.g. 0755</HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FieldPopover>
      </StackItem>

      <StackItem>
        <FieldPopover
          fieldId="fsp-uid"
          label="Default UID"
          description="Owner UID for new subdirectories. Set to 107 for KubeVirt VMs (QEMU user). Leave empty to use the pool default (root, squashed to 65534 by NFS)."
        >
          <TextInput
            id="fsp-uid"
            value={form.defaultUID}
            onChange={(_event, value) => set('defaultUID', value)}
            placeholder=""
            type="number"
          />
          <FormHelperText>
            <HelperText>
              <HelperTextItem>Optional, e.g. 107 for KubeVirt</HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FieldPopover>
      </StackItem>

      <StackItem>
        <FieldPopover
          fieldId="fsp-gid"
          label="Default GID"
          description="Owner GID for new subdirectories. Typically matches the UID setting. Set to 107 for KubeVirt VMs."
        >
          <TextInput
            id="fsp-gid"
            value={form.defaultGID}
            onChange={(_event, value) => set('defaultGID', value)}
            placeholder=""
            type="number"
          />
          <FormHelperText>
            <HelperText>
              <HelperTextItem>Optional</HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FieldPopover>
      </StackItem>

      <StackItem>
        <FieldPopover
          fieldId="fsp-encryption"
          label="Encryption in Transit"
          description="Enable TLS encryption for NFS traffic between worker nodes and VPC file shares. Adds ~5-10% latency overhead but protects data in transit. Not supported on bare metal worker nodes."
        >
          <Switch
            id="fsp-encryption"
            label="Encryption in transit"
            isChecked={form.encryptionInTransit}
            onChange={(_event, checked) => set('encryptionInTransit', checked)}
          />
          <FormHelperText>
            <HelperText>
              <HelperTextItem variant={form.encryptionInTransit ? 'warning' : 'default'}>
                {form.encryptionInTransit
                  ? 'Not supported on bare metal workers — virtual server instances only'
                  : 'Virtual server instances only (not bare metal)'}
              </HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FieldPopover>
      </StackItem>
    </Stack>
  );

  const advancedStep = (
    <Stack hasGutter>
      <StackItem>
        <FieldPopover
          fieldId="fsp-resource-group"
          label="Resource Group"
          description="VPC resource group ID for organizing file shares. If empty, shares are created in the account default resource group."
        >
          <TextInput
            id="fsp-resource-group"
            value={form.resourceGroup}
            onChange={(_event, value) => set('resourceGroup', value)}
            placeholder=""
          />
          <FormHelperText>
            <HelperText>
              <HelperTextItem>Optional VPC resource group ID</HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FieldPopover>
      </StackItem>

      <StackItem>
        <FieldPopover
          fieldId="fsp-tags"
          label="Tags"
          description="Tags applied to VPC file shares for cost tracking and organization. One tag per line, e.g. env:production. Tags are propagated to the VPC API."
        >
          <TextArea
            id="fsp-tags"
            value={form.tags}
            onChange={(_event, value) => set('tags', value)}
            rows={4}
            placeholder={"env:production\nteam:storage"}
          />
          <FormHelperText>
            <HelperText>
              <HelperTextItem>One tag per line (optional)</HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FieldPopover>
      </StackItem>

      <StackItem>
        <FieldPopover
          fieldId="fsp-mount-options"
          label="Mount Options"
          description="NFS mount options passed to worker node mounts. Defaults: soft,timeo=600,retrans=3,sec=sys. Never use 'hard' — pods hang indefinitely on NFS failures."
        >
          <TextArea
            id="fsp-mount-options"
            value={form.mountOptions}
            onChange={(_event, value) => set('mountOptions', value)}
            rows={4}
            placeholder={"soft\ntimeo=600\nretrans=3"}
          />
          <FormHelperText>
            <HelperText>
              <HelperTextItem>One option per line (optional)</HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FieldPopover>
      </StackItem>
    </Stack>
  );

  const goldenImagesStep = (
    <Stack hasGutter>
      <StackItem>
        <FieldPopover
          fieldId="fsp-golden-enabled"
          label="Golden Image Syncer"
          description="Automatically discover DataImportCron resources and provision ready-to-boot VM golden images on pool storage. Converts qcow2 images to raw format for KubeVirt."
        >
          <Switch
            id="fsp-golden-enabled"
            label="Enable golden image syncer"
            isChecked={form.goldenImagesEnabled}
            onChange={(_event, checked) => set('goldenImagesEnabled', checked)}
          />
        </FieldPopover>
      </StackItem>

      {form.goldenImagesEnabled && (
        <>
          <StackItem>
            <FieldPopover
              fieldId="fsp-golden-namespaces"
              label="Target Namespaces"
              description="Namespaces where golden image PVCs will be created. Typically openshift-virtualization-os-images for KubeVirt. One namespace per line."
              isRequired
            >
              <TextArea
                id="fsp-golden-namespaces"
                value={form.targetNamespaces}
                onChange={(_event, value) => set('targetNamespaces', value)}
                rows={3}
                placeholder={"openshift-virtualization-os-images\ndefault"}
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>One namespace per line</HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="fsp-golden-filter"
              label="Image Filter"
              description="Glob patterns to filter which DataImportCron images to sync. Leave empty to sync all discovered images. Example: fedora-*, centos-stream*."
            >
              <TextArea
                id="fsp-golden-filter"
                value={form.imageFilter}
                onChange={(_event, value) => set('imageFilter', value)}
                rows={3}
                placeholder={"fedora-*\ncentos-stream*"}
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>One filter pattern per line (optional)</HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="fsp-golden-interval"
              label="Refresh Interval"
              description="How often the syncer checks for new or updated golden images. Shorter intervals catch upstream image updates faster but increase API load."
            >
              <TextInput
                id="fsp-golden-interval"
                value={form.refreshInterval}
                onChange={(_event, value) => set('refreshInterval', value)}
                placeholder="24h"
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>Go duration, e.g. 24h, 6h, 30m</HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="fsp-golden-converter"
              label="Converter Image"
              description="Container image used for qcow2-to-raw conversion Jobs. Must have qemu-img installed. Default uses CentOS Stream 9 which includes qemu-img in its repos."
            >
              <TextInput
                id="fsp-golden-converter"
                value={form.converterImage}
                onChange={(_event, value) => set('converterImage', value)}
                placeholder="quay.io/centos/centos:stream9"
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>Container image used for qcow2-to-raw conversion</HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="fsp-golden-pvc-size"
              label="PVC Size (GB)"
              description="Size of each golden image PVC. Must be large enough to hold the converted raw image. Typical cloud images are 2-10 GB raw. Minimum 5 GB."
              isRequired
            >
              <NumberInput
                id="fsp-golden-pvc-size"
                value={form.pvcSizeGB}
                min={5}
                onMinus={() => set('pvcSizeGB', Math.max(5, form.pvcSizeGB - 1))}
                onPlus={() => set('pvcSizeGB', form.pvcSizeGB + 1)}
                onChange={(event) => {
                  const val = parseInt((event.target as HTMLInputElement).value, 10);
                  if (!isNaN(val)) set('pvcSizeGB', val);
                }}
              />
            </FieldPopover>
          </StackItem>
        </>
      )}
    </Stack>
  );

  const resource = buildResource(form);
  const yamlPreview = toYAML(resource);

  const reviewStep = (
    <Stack hasGutter>
      {error && (
        <StackItem>
          <Alert variant="danger" title="Creation failed" isInline>
            {error}
          </Alert>
        </StackItem>
      )}

      {/* Summary card */}
      <StackItem>
        <Card>
          <CardTitle>Summary</CardTitle>
          <CardBody>
            <Grid hasGutter>
              <GridItem span={6}>
                <DescriptionList isCompact isHorizontal>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Name</DescriptionListTerm>
                    <DescriptionListDescription>{form.name || '-'}</DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Zone</DescriptionListTerm>
                    <DescriptionListDescription>{form.zone || '-'}</DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Profile</DescriptionListTerm>
                    <DescriptionListDescription>
                      {form.profile}{form.profile === 'custom' ? ` (${form.iops} IOPS)` : ''}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Share Size</DescriptionListTerm>
                    <DescriptionListDescription>{form.shareSizeGB} GB</DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
              </GridItem>
              <GridItem span={6}>
                <DescriptionList isCompact isHorizontal>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Max Capacity</DescriptionListTerm>
                    <DescriptionListDescription>
                      <Label color="blue">{maxCapacity.toLocaleString()} GB</Label>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Shares</DescriptionListTerm>
                    <DescriptionListDescription>
                      {form.initialShares} initial / {form.maxShares} max
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Strategy</DescriptionListTerm>
                    <DescriptionListDescription>
                      <Label>{form.allocationStrategy}</Label>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Golden Images</DescriptionListTerm>
                    <DescriptionListDescription>
                      <Label color={form.goldenImagesEnabled ? 'green' : 'grey'}>
                        {form.goldenImagesEnabled ? 'Enabled' : 'Disabled'}
                      </Label>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
              </GridItem>
            </Grid>
          </CardBody>
        </Card>
      </StackItem>

      <StackItem>
        <YAMLEditorFallback yaml={yamlPreview} />
      </StackItem>

      <StackItem>
        <ActionGroup>
          <Button
            variant="primary"
            onClick={handleCreate}
            isDisabled={creating || !isBasicValid || !isPoolSizingValid || !isGoldenImagesValid}
            isLoading={creating}
          >
            Create
          </Button>
          <Button variant="link" onClick={() => history.goBack()} isDisabled={creating}>
            Cancel
          </Button>
        </ActionGroup>
      </StackItem>
    </Stack>
  );

  return (
    <PageSection variant="default">
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Create FileSharePool
      </Title>
      <Wizard>
        <WizardStep name="Basic" id="step-basic">
          {basicStep}
        </WizardStep>
        <WizardStep
          name="Pool Sizing"
          id="step-pool-sizing"
          isDisabled={!isBasicValid}
        >
          {poolSizingStep}
        </WizardStep>
        <WizardStep
          name="Permissions"
          id="step-permissions"
          isDisabled={!isBasicValid || !isPoolSizingValid}
        >
          {permissionsStep}
        </WizardStep>
        <WizardStep
          name="Advanced"
          id="step-advanced"
          isDisabled={!isBasicValid || !isPoolSizingValid}
        >
          {advancedStep}
        </WizardStep>
        <WizardStep
          name="Golden Images"
          id="step-golden-images"
          isDisabled={!isBasicValid || !isPoolSizingValid}
        >
          {goldenImagesStep}
        </WizardStep>
        <WizardStep
          name="Review"
          id="step-review"
          isDisabled={!isBasicValid || !isPoolSizingValid || !isGoldenImagesValid}
          footer={{ isNextDisabled: true }}
        >
          {reviewStep}
        </WizardStep>
      </Wizard>
    </PageSection>
  );
};

export default FileSharePoolCreatePage;
