import extensions from '../console-extensions.json';

type Extension = { type: string; properties: Record<string, unknown> };

const navEntries = (extensions as Extension[]).filter((e) => e.type === 'console.navigation/href');

describe('console navigation', () => {
  it.each([
    ['admin', 'storage'],
    ['virtualization-perspective', 'storage-virt-perspective'],
  ])('links the pools page from the %s perspective', (perspective, section) => {
    const entry = navEntries.find((e) => e.properties.perspective === perspective);
    expect(entry?.properties).toMatchObject({ href: '/vpc-file-pools', section });
  });
});
