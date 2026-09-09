export const emptyCombo = () => ({
  name: '', models: [], strategy: 'fallback', 'display-name': '',
  'sticky-round-robin-limit': 1, 'judge-model': '', 'judge-prompt': '',
  'min-successful-models': 1, disabled: false,
});

export function normalizeCombo(combo) {
  return {
    ...emptyCombo(), ...combo,
    name: String(combo.name || '').trim(),
    models: Array.isArray(combo.models) ? [...combo.models] : [],
    'display-name': combo['display-name'] ?? combo.displayName ?? '',
    'sticky-round-robin-limit': combo['sticky-round-robin-limit'] ?? combo.stickyRoundRobinLimit ?? 1,
    'judge-model': combo['judge-model'] ?? combo.judgeModel ?? '',
    'judge-prompt': combo['judge-prompt'] ?? combo.judgePrompt ?? '',
    'min-successful-models': combo['min-successful-models'] || combo.minSuccessfulModels || 1,
  };
}

export function splitMember(value) {
  const index = value.indexOf('::');
  return index < 0
    ? { provider: '', model: value }
    : { provider: value.slice(0, index), model: value.slice(index + 2) };
}

export function addMember(models, value) {
  const trimmed = value.trim();
  return !trimmed || models.some(model => model.toLowerCase() === trimmed.toLowerCase())
    ? models : [...models, trimmed];
}

export function moveMember(models, index, offset) {
  const to = index + offset;
  if (index < 0 || index >= models.length || to < 0 || to >= models.length) return models;
  const next = [...models];
  [next[index], next[to]] = [next[to], next[index]];
  return next;
}

export function filterCatalog(providers, provider, query) {
  const needle = query.trim().toLowerCase();
  return providers.filter(group => !provider || group.id === provider)
    .flatMap(group => group.models.map(model => ({ ...model, provider: group.id, providerName: group.name || group.id })))
    .filter(model => `${model.providerName} ${model.provider} ${model.id} ${model.name}`.toLowerCase().includes(needle));
}

export function comboValidation(combo) {
  if (!combo.name.trim() || !combo.models.length) return 'required';
  if (!['fallback', 'round-robin', 'fusion'].includes(combo.strategy)) return 'strategy';
  if (combo.models.some(model => model.toLowerCase() === combo.name.trim().toLowerCase())) return 'nested';
  if (combo.strategy === 'fusion') {
    if (!combo['judge-model'].trim()) return 'judgeRequired';
    if (combo.models.length > 16) return 'panelLimit';
    const minimum = Number(combo['min-successful-models']);
    if (!Number.isInteger(minimum) || minimum < 1 || minimum > combo.models.length) return 'minimumInvalid';
  }
  const sticky = Number(combo['sticky-round-robin-limit']);
  if (!Number.isInteger(sticky) || sticky < 1) return 'stickyInvalid';
  return '';
}
