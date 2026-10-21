import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sDelete } from '@openshift-console/dynamic-plugin-sdk';
import DeleteModal from './DeleteModal';

const model = { apiVersion: 'v1', kind: 'Thing', plural: 'things', abbr: 'T', label: 'Thing', labelPlural: 'Things' };

const setup = (extraOption?: React.ComponentProps<typeof DeleteModal>['extraOption']) => {
  const props = { isOpen: true, resourceName: 'dr', resourceKind: 'Thing', model, onClose: jest.fn(), onDeleted: jest.fn(), extraOption };
  render(<DeleteModal {...props} />);
  return props;
};

beforeEach(() => {
  jest.clearAllMocks();
  (k8sDelete as jest.Mock).mockResolvedValue({});
});

describe('DeleteModal extra option', () => {
  it('shows no extra option unless one is given', () => {
    setup();
    expect(screen.queryByRole('checkbox')).toBeNull();
  });

  it('is off by default, and then does nothing extra', async () => {
    const run = jest.fn().mockResolvedValue(undefined);
    const props = setup({ label: 'Also delete the Secrets', run });
    expect((screen.getByRole('checkbox', { name: /Also delete the Secrets/ }) as HTMLInputElement).checked).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(props.onDeleted).toHaveBeenCalled());
    expect(run).not.toHaveBeenCalled();
  });

  it('runs the extra step after the delete when it is ticked', async () => {
    const order: string[] = [];
    (k8sDelete as jest.Mock).mockImplementation(async () => {
      order.push('delete');
    });
    const run = jest.fn(async () => {
      order.push('extra');
    });
    const props = setup({ label: 'Also delete the Secrets', run });
    fireEvent.click(screen.getByRole('checkbox', { name: /Also delete the Secrets/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(props.onDeleted).toHaveBeenCalled());
    expect(order).toEqual(['delete', 'extra']);
  });

  it('says the resource was deleted when the extra step fails, and carries on at Continue', async () => {
    const run = jest.fn().mockRejectedValue(new Error('a was not made by the console'));
    const props = setup({ label: 'Also delete the Secrets', run });
    fireEvent.click(screen.getByRole('checkbox', { name: /Also delete the Secrets/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    expect(await screen.findByText(/was deleted, but/)).toBeTruthy();
    expect(screen.getByText(/a was not made by the console/)).toBeTruthy();
    expect(props.onDeleted).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(props.onDeleted).toHaveBeenCalled();
  });
});
