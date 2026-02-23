/**
 * Module augmentation for @openshift-console/dynamic-plugin-sdk.
 *
 * In SDK 4.20.0 several component prop types do not include `children`,
 * and React 18 removed the implicit `children` prop from React.FC.
 * However, the SDK's own JSDoc examples show these components used with
 * children (e.g. TableData wrapping ResourceLink, ListPageCreate with
 * button text, ListPageBody wrapping content). This augmentation
 * re-declares the component signatures with PropsWithChildren so that
 * existing usage compiles correctly.
 */
import * as React from 'react';
import {
  TableDataProps,
  ListPageCreateProps,
  ListPageCreateLinkProps,
} from '@openshift-console/dynamic-plugin-sdk/lib/extensions/console-types';

declare module '@openshift-console/dynamic-plugin-sdk' {
  export const TableData: React.FC<React.PropsWithChildren<TableDataProps>>;
  export const ListPageCreate: React.FC<React.PropsWithChildren<ListPageCreateProps>>;
  export const ListPageCreateLink: React.FC<React.PropsWithChildren<ListPageCreateLinkProps>>;
  export const ListPageBody: React.FC<React.PropsWithChildren<{}>>;
}
