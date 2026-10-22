import React from 'react';
import { render, screen } from '@testing-library/react';
import NoAccessNote from './NoAccessNote';

describe('NoAccessNote', () => {
  it('names the missing right and shows the YAML for an administrator', () => {
    render(<NoAccessNote right="create BackupRestore objects" yaml={'kind: BackupRestore\n'} />);
    expect(screen.getByText(/You are not allowed to create BackupRestore objects/)).toBeTruthy();
    expect(screen.getByText(/Give this YAML to an administrator/)).toBeTruthy();
    expect(screen.getByText(/kind: BackupRestore/)).toBeTruthy();
  });

  it('shows no YAML when there is none', () => {
    render(<NoAccessNote right="delete Jobs" />);
    expect(screen.getByText(/You are not allowed to delete Jobs/)).toBeTruthy();
    expect(screen.queryByText(/Give this YAML/)).toBeNull();
  });
});
