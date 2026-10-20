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

// Golden images are hidden: VMs come from the standard catalog (spec
// 2026-10-20-cdi-copy-limit). The form never turns them on.
describe('golden images are hidden', () => {
  it.each(PRESETS.map((p): [string, Record<string, unknown>] => [p.id, p.values]))(
    'preset %s does not offer golden images',
    (_id, values) => {
      expect(Object.keys(values).some((k) => /golden|refreshInterval|pvcSizeGB/.test(k))).toBe(false);
    },
  );

  it('a pool made by the form has no golden images', () => {
    for (const preset of PRESETS) {
      expect(buildResource(form(preset.values)).spec.goldenImages).toBeUndefined();
    }
  });
});
