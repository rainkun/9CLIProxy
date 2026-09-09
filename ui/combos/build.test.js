import { expect, test } from 'bun:test';
import { readFile } from 'node:fs/promises';
import { patchBundle } from './build.mjs';

test('combo extension is idempotent and preserves the rest of the bundle', () => {
  const html = '<html><head><script type="module">function yte(){return "old"};var marker="connectionStatus showNotification showConfirmation sp.patch( sp.get(";use(yte);</script></head></html>';
  const first = patchBundle(html, 'function createCombosPage(){}', '.test{}');
  const second = patchBundle(first, 'function createCombosPage(){}', '.test{}');
  expect(second).toBe(first);
  expect((second.match(/function yte\(/g)||[])).toHaveLength(1);
  expect(second).toContain('function legacyCombosPage(){return "old"}');
  expect(second).toContain('var cliProxyCombosPage=(()=>{');
});

test('unsupported upstream bundles fail closed', () => {
  expect(()=>patchBundle('<html><head><script>other()</script></head></html>', '', '')).toThrow();
});

test('both shipped UI copies include the same provider picker and Fusion page', async () => {
  const web = await readFile(new URL('../../static/management.html', import.meta.url), 'utf8');
  const desktop = await readFile(new URL('../../internal/desktop/management.html', import.meta.url), 'utf8');
  expect(web).toBe(desktop);
  for (const marker of ['combo-judge-model','combo-provider','/combos/models','createCombosPage']) expect(web).toContain(marker);
});
