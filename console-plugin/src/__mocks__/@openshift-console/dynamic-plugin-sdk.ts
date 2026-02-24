import React from 'react';

// K8s watch hook mock
export const useK8sWatchResource = jest.fn(() => [[], true, undefined]);

// K8s CRUD mocks
export const k8sCreate = jest.fn();
export const k8sDelete = jest.fn();
export const k8sPatch = jest.fn();
export const k8sUpdate = jest.fn();

// Console fetch mock
export const consoleFetchJSON = jest.fn();

// List page components — render children or placeholder
export const ListPageHeader: React.FC<{ title?: string; children?: React.ReactNode }> = ({
  title,
  children,
}) => React.createElement('div', { 'data-testid': 'list-page-header' }, title, children);

export const ListPageBody: React.FC<{ children?: React.ReactNode }> = ({ children }) =>
  React.createElement('div', { 'data-testid': 'list-page-body' }, children);

export const ListPageFilter: React.FC = () =>
  React.createElement('div', { 'data-testid': 'list-page-filter' });

export const ListPageCreateLink: React.FC<{ to?: string; children?: React.ReactNode }> = ({
  children,
}) => React.createElement('a', { 'data-testid': 'list-page-create-link' }, children);

export const useListPageFilter = jest.fn((data) => [data, data, jest.fn()]);

// Timestamp — render text
export const Timestamp: React.FC<{ timestamp?: string }> = ({ timestamp }) =>
  React.createElement('span', { 'data-testid': 'timestamp' }, timestamp);

// K8sResourceCommon type
export type K8sResourceCommon = {
  apiVersion?: string;
  kind?: string;
  metadata?: {
    name?: string;
    namespace?: string;
    uid?: string;
    creationTimestamp?: string;
    labels?: Record<string, string>;
    annotations?: Record<string, string>;
    resourceVersion?: string;
  };
};
