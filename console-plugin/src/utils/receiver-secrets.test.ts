import { k8sDelete, k8sPatch } from '@openshift-console/dynamic-plugin-sdk';
import { deleteMadeSecrets } from './receiver-secrets';

beforeEach(() => {
  jest.clearAllMocks();
  (k8sPatch as jest.Mock).mockResolvedValue({});
  (k8sDelete as jest.Mock).mockResolvedValue({});
});

describe('deleteMadeSecrets', () => {
  it('checks that the console made the Secret, then deletes it', async () => {
    await deleteMadeSecrets(['dr-receiver-token']);
    expect((k8sPatch as jest.Mock).mock.calls[0][0].data).toEqual([
      { op: 'test', path: '/metadata/labels/vpc-file-pool-csi.ibm.io~1made-by-console', value: 'true' },
    ]);
    expect((k8sPatch as jest.Mock).mock.calls[0][0].resource.metadata).toEqual({ name: 'dr-receiver-token', namespace: 'kube-system' });
    expect(k8sDelete).toHaveBeenCalledTimes(1);
    expect((k8sDelete as jest.Mock).mock.calls[0][0].resource.metadata).toEqual({ name: 'dr-receiver-token', namespace: 'kube-system' });
  });

  it('leaves a Secret alone that the console did not make, and says so', async () => {
    (k8sPatch as jest.Mock).mockRejectedValue({ code: 422 });
    await expect(deleteMadeSecrets(['dr-receiver-token'])).rejects.toThrow(/dr-receiver-token.*not made by the console/);
    expect(k8sDelete).not.toHaveBeenCalled();
  });

  it('is fine when a Secret is already gone', async () => {
    (k8sPatch as jest.Mock).mockRejectedValue({ code: 404 });
    await expect(deleteMadeSecrets(['dr-receiver-token'])).resolves.toBeUndefined();
    expect(k8sDelete).not.toHaveBeenCalled();
  });

  it('goes on to the next Secret when one is left alone, and reports them all', async () => {
    (k8sPatch as jest.Mock).mockRejectedValueOnce({ code: 422 }).mockResolvedValueOnce({});
    await expect(deleteMadeSecrets(['a', 'b'])).rejects.toThrow(/a/);
    expect(k8sDelete).toHaveBeenCalledTimes(1);
  });
});
