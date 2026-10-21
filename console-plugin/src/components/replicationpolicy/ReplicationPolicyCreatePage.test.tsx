import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import ReplicationPolicyCreatePage from './ReplicationPolicyCreatePage';

jest.mock('react-router-dom', () => ({ useHistory: () => ({ push: jest.fn() }) }));

// A stand-in for the receiver setup: it lets the test fill the fields and pass or fail the check.
jest.mock('./ReceiverSetup', () => ({
  __esModule: true,
  default: (props: {
    onEndpointChange: (v: string) => void;
    onAuthSecretRefChange: (v: string) => void;
    onCheckedChange: (v: boolean) => void;
  }) => (
    <div>
      <button
        onClick={() => {
          props.onEndpointChange('https://repl.example.com');
          props.onAuthSecretRefChange('dr-receiver-token');
        }}
      >
        fill receiver
      </button>
      <button onClick={() => props.onCheckedChange(true)}>pass check</button>
    </div>
  ),
}));

beforeEach(() => {
  (useK8sWatchResource as jest.Mock).mockReturnValue([[{ metadata: { name: 'pool-a' }, status: { totalPVCCount: 1 } }], true, undefined]);
});

const createButton = () => screen.getByRole('button', { name: /^Create$/ }) as HTMLButtonElement;

function fillPolicy() {
  fireEvent.change(screen.getByLabelText(/^Name( \*)?$/), { target: { value: 'dr' } });
  fireEvent.change(screen.getByLabelText(/^Source Pool Name( \*)?$/), { target: { value: 'pool-a' } });
}

const useReceiver = () => fireEvent.change(screen.getByLabelText(/^Replication Mode( \*)?$/), { target: { value: 'driver-to-driver' } });

describe('ReplicationPolicyCreatePage receiver mode', () => {
  it('keeps Create off until the receiver check has passed', () => {
    render(<ReplicationPolicyCreatePage />);
    fillPolicy();
    useReceiver();
    fireEvent.click(screen.getByText('fill receiver'));
    expect(createButton().disabled).toBe(true);
    fireEvent.click(screen.getByText('pass check'));
    expect(createButton().disabled).toBe(false);
  });

  it('asks for the check again after the mode is switched away and back', () => {
    render(<ReplicationPolicyCreatePage />);
    fillPolicy();
    useReceiver();
    fireEvent.click(screen.getByText('fill receiver'));
    fireEvent.click(screen.getByText('pass check'));
    fireEvent.change(screen.getByLabelText(/^Replication Mode( \*)?$/), { target: { value: 'direct' } });
    useReceiver();
    expect(createButton().disabled).toBe(true);
  });
});
