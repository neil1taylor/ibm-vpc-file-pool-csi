import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { fetchRawCSV, fetchVMLines } from './poolbench-raw';

const reply = (body: string, contentType: string) =>
  ({ text: () => Promise.resolve(body), headers: { get: () => contentType } } as unknown as Response);

describe('poolbench raw results', () => {
  it('reads the lines of a VM as text when the service adds a charset', async () => {
    (consoleFetch as jest.Mock).mockResolvedValue(reply('PERF-READY\nPERF 1 steady {}\n', 'text/plain; charset=utf-8'));
    await expect(fetchVMLines('t3000', 'vm-001')).resolves.toBe('PERF-READY\nPERF 1 steady {}\n');
    expect((consoleFetch as jest.Mock).mock.calls[0][0]).toBe(
      '/api/proxy/plugin/ibm-vpc-file-pool-csi/poolbench/v1/runs/t3000/vms/vm-001/log',
    );
  });

  it('reads the CSV as text', async () => {
    (consoleFetch as jest.Mock).mockResolvedValue(reply('vm,os\nvm-001,linux\n', 'text/csv; charset=utf-8'));
    await expect(fetchRawCSV('t3000')).resolves.toBe('vm,os\nvm-001,linux\n');
  });
});
