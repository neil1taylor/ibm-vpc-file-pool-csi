import { ReplicationPolicySpec } from '../types';

interface DestinationInput {
  isDriverToDriver: boolean;
  nfsServer: string;
  endpoint: string;
  authSecretRef: string;
  caCertSecretRef: string;
}

/** The destination fields of a policy; only those that apply to the chosen mode. */
export function destinationSpecFields(input: DestinationInput): Partial<ReplicationPolicySpec> {
  if (!input.isDriverToDriver) {
    return { destinationNFSServer: input.nfsServer };
  }
  const fields: Partial<ReplicationPolicySpec> = {
    destinationEndpoint: input.endpoint,
    destinationAuthSecretRef: input.authSecretRef,
  };
  const ca = input.caCertSecretRef.trim();
  if (ca) fields.destinationCACertSecretRef = ca;
  return fields;
}

export function replicationDestination(spec: ReplicationPolicySpec | undefined): string {
  if (!spec) return '-';
  if (spec.destinationEndpoint) return spec.destinationEndpoint;
  if (spec.destinationNFSServer) {
    return `${spec.destinationNFSServer}:${spec.destinationBasePath || '/pvcs'}`;
  }
  return '-';
}
