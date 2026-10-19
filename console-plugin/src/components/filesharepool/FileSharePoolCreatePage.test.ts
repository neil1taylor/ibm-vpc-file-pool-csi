import { buildResource, initialFormState, PRESETS } from './FileSharePoolCreatePage';

const form = (overrides = {}) => ({ ...initialFormState, name: 'p', zone: 'eu-de-3', ...overrides });

describe('buildResource', () => {
  it('omits iops when it is 0, so the VPC chooses it', () => {
    expect(buildResource(form({ iops: 0 })).spec.iops).toBeUndefined();
  });

  it('sets iops for a dp2 pool', () => {
    const pool = buildResource(form({ profile: 'dp2', iops: 3000 }));
    expect(pool.spec).toMatchObject({ profile: 'dp2', iops: 3000 });
  });
});

describe('PRESETS', () => {
  // dp2 is the only VPC file share profile (ibmcloud is share-profiles, eu-de, Oct 2026).
  it.each(PRESETS.map((p): [string, string | undefined] => [p.id, p.values.profile]))(
    '%s uses the dp2 profile',
    (_id, profile) => {
      expect(profile).toBe('dp2');
    },
  );
});
