import { readFile, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { resolve, dirname } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, '../..');
const targets = [resolve(root, 'static/management.html'), resolve(root, 'internal/desktop/management.html')];
const scriptStart = '/* CLIProxy combos v2:start */';
const scriptEnd = '/* CLIProxy combos v2:end */';
const cssStart = '<style id="cliproxy-combos-v2">';

// The repository vendors a single-file UI, not its upstream source tree.
// Keep the rest of that UI byte-for-byte and replace only its Combo page.
// Fail closed if upstream renames the pinned adapter symbols.
export function patchBundle(html, script, css) {
  const begin = html.indexOf(scriptStart);
  if (begin >= 0) {
    const end = html.indexOf(scriptEnd, begin);
    if (end < 0) throw new Error('Incomplete combo script marker');
    html = html.slice(0, begin) + html.slice(end + scriptEnd.length);
  }
  const cssBegin = html.indexOf(cssStart);
  if (cssBegin >= 0) {
    const cssEnd = html.indexOf('</style>', cssBegin);
    if (cssEnd < 0) throw new Error('Incomplete combo CSS marker');
    html = html.slice(0, cssBegin) + html.slice(cssEnd + 8);
  }
  if (html.includes('function yte(){')) html = html.replace('function yte(){', 'function legacyCombosPage(){');
  if (!html.includes('function legacyCombosPage(){') || !html.includes('yte')) {
    throw new Error('Unsupported management bundle: Combo route adapter not found');
  }
  for (const marker of ['connectionStatus', 'showNotification', 'showConfirmation', 'sp.patch(', 'sp.get(']) {
    if (!html.includes(marker)) throw new Error(`Unsupported management bundle: missing ${marker}`);
  }
  const insertion = html.indexOf('</script>');
  if (insertion < 0) throw new Error('Management module script not found');
  const extension = `${scriptStart}\nvar cliProxyCombosPage=(()=>{${script}\nreturn createCombosPage(y,sp,om,qc,V);})();function yte(){return y.createElement(cliProxyCombosPage);}\n${scriptEnd}`;
  html = html.slice(0, insertion) + extension + html.slice(insertion);
  return html.replace('</head>', `${cssStart}${css}</style></head>`);
}

if (import.meta.main) {
  const result = await Bun.build({
    entrypoints: [resolve(here, 'CombosPage.jsx')],
    target: 'browser', format: 'esm',
    minify: { whitespace: true, identifiers: false, syntax: false },
  });
  if (!result.success) throw new AggregateError(result.logs, 'Combo UI compilation failed');
  let script = await result.outputs[0].text();
  script = script.replace(/export\s*\{[^}]*\};?\s*$/, '');
  if (/\bimport\s/.test(script) || /\bexport\s*\{/.test(script)) throw new Error('Combo page must be self-contained');
  const css = await readFile(resolve(here, 'combos.css'), 'utf8');
  const originals = await Promise.all(targets.map(path => readFile(path, 'utf8')));
  if (originals[0] !== originals[1]) throw new Error('Static and desktop UI differ; reconcile before building');
  const output = patchBundle(originals[0], script, css);
  for (const path of targets) await writeFile(path, output);
  console.log('Updated static and embedded management UI from ui/combos source.');
}
