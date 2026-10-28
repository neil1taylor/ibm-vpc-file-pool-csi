import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { DriverSettings } from './backup-helpers';
import { SETTINGS_CONFIGMAP, SettingsConfigMapLike } from './instant-snapshots';

/** The driver's settings ConfigMap, or undefined while it loads or when the person cannot read it. */
export function useDriverSettings(): DriverSettings | undefined {
  const [settings] = useK8sWatchResource<SettingsConfigMapLike>({
    groupVersionKind: { version: 'v1', kind: 'ConfigMap' },
    name: SETTINGS_CONFIGMAP.name,
    namespace: SETTINGS_CONFIGMAP.namespace,
  });
  return settings;
}
