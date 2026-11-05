import * as fs from 'fs';
import * as path from 'path';
import * as ts from 'typescript';

const SRC = path.resolve(__dirname, '..');

function listSources(dir: string): string[] {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) return entry.name === '__mocks__' ? [] : listSources(full);
    return /\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name) && !entry.name.endsWith('.d.ts')
      ? [full]
      : [];
  });
}

function resolveModule(from: string, spec: string): string | undefined {
  let base: string;
  if (spec.startsWith('@/')) base = path.join(SRC, spec.slice(2));
  else if (spec.startsWith('.')) base = path.resolve(path.dirname(from), spec);
  else return undefined;
  const candidates = [base + '.ts', base + '.tsx', path.join(base, 'index.ts'), path.join(base, 'index.tsx')];
  return candidates.find((c) => fs.existsSync(c));
}

/** Modules a file loads at run time. Imports that are only types are erased by the build and do not count. */
function runtimeImports(file: string): string[] {
  const source = ts.createSourceFile(file, fs.readFileSync(file, 'utf8'), ts.ScriptTarget.Latest, true);
  const found: string[] = [];
  for (const node of source.statements) {
    let spec: string | undefined;
    if (ts.isImportDeclaration(node)) {
      const clause = node.importClause;
      const typeOnly =
        clause &&
        (clause.isTypeOnly ||
          (!clause.name &&
            clause.namedBindings &&
            ts.isNamedImports(clause.namedBindings) &&
            clause.namedBindings.elements.length > 0 &&
            clause.namedBindings.elements.every((e) => e.isTypeOnly)));
      if (!typeOnly) spec = (node.moduleSpecifier as ts.StringLiteral).text;
    } else if (ts.isExportDeclaration(node) && node.moduleSpecifier && !node.isTypeOnly) {
      spec = (node.moduleSpecifier as ts.StringLiteral).text;
    }
    const target = spec && resolveModule(file, spec);
    if (target) found.push(target);
  }
  return found;
}

function findCycles(): string[][] {
  const graph = new Map<string, string[]>();
  for (const file of listSources(SRC)) graph.set(file, runtimeImports(file));
  const cycles: string[][] = [];
  const state = new Map<string, 'open' | 'done'>();
  const stack: string[] = [];
  const visit = (file: string) => {
    state.set(file, 'open');
    stack.push(file);
    for (const next of graph.get(file) || []) {
      if (state.get(next) === 'open') cycles.push([...stack.slice(stack.indexOf(next)), next]);
      else if (!state.has(next)) visit(next);
    }
    stack.pop();
    state.set(file, 'done');
  };
  for (const file of graph.keys()) if (!state.has(file)) visit(file);
  return cycles;
}

describe('import cycles', () => {
  it('no module of the plugin is part of an import cycle', () => {
    const cycles = findCycles().map((c) => c.map((f) => path.relative(SRC, f)).join(' -> '));
    expect(cycles).toEqual([]);
  });
});
