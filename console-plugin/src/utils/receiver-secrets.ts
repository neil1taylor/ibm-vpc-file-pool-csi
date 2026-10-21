import { k8sDelete, k8sPatch } from '@openshift-console/dynamic-plugin-sdk';
import { SecretModel } from '../models';
import { LABEL_MADE_BY_CONSOLE, RECEIVER_NAMESPACE } from './receiver-objects';

const errorCode = (err: unknown): number | undefined => {
  const e = err as { code?: number; response?: { status?: number }; json?: { code?: number } };
  return e?.code ?? e?.response?.status ?? e?.json?.code;
};

/**
 * Deletes Secrets the receiver setup made. A Secret is deleted only after a test patch proves it has the label of
 * the console (the patch changes nothing and does not read the Secret). One that lacks the label is left alone.
 */
export async function deleteMadeSecrets(names: string[]): Promise<void> {
  const leftAlone: string[] = [];
  for (const name of names) {
    const resource = { metadata: { name, namespace: RECEIVER_NAMESPACE } };
    try {
      await k8sPatch({
        model: SecretModel,
        resource,
        data: [{ op: 'test', path: `/metadata/labels/${LABEL_MADE_BY_CONSOLE.replace('/', '~1')}`, value: 'true' }],
      });
    } catch (err) {
      const code = errorCode(err);
      if (code === 404) continue;
      if (code === 422) {
        leftAlone.push(name);
        continue;
      }
      throw err;
    }
    try {
      await k8sDelete({ model: SecretModel, resource });
    } catch (err) {
      if (errorCode(err) !== 404) throw err;
    }
  }
  if (leftAlone.length > 0) {
    throw new Error(`The Secret ${leftAlone.join(', ')} was not made by the console, so it was left alone.`);
  }
}
