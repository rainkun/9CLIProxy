import { describe, expect, test } from 'bun:test';
import { addMember, comboValidation, emptyCombo, filterCatalog, moveMember, normalizeCombo, splitMember } from './model-options.js';
import { comboLocale } from './locales.js';

const catalog = [
  { id: 'provider-a', models: [{ id: 'shared', name: 'Shared', value: 'provider-a::shared' }] },
  { id: 'provider-b', models: [{ id: 'shared', name: 'Shared', value: 'provider-b::shared' }] },
];
describe('Combo provider/model selection', () => {
  test('same-name models stay pinned to the selected provider', () => {
    expect(filterCatalog(catalog, 'provider-b', 'shared').map(item => item.value)).toEqual(['provider-b::shared']);
    expect(addMember(['provider-a::shared'], 'provider-b::shared')).toHaveLength(2);
    expect(addMember(['provider-a::shared'], 'provider-a::shared')).toHaveLength(1);
  });
  test('priority reordering and bounds', () => {
    expect(moveMember(['a', 'b', 'c'], 1, -1)).toEqual(['b', 'a', 'c']);
    expect(moveMember(['a', 'b'], 0, -1)).toEqual(['a', 'b']);
    expect(splitMember('provider::org/model')).toEqual({ provider: 'provider', model: 'org/model' });
    expect(splitMember('org/model')).toEqual({ provider: '', model: 'org/model' });
  });
  test('fusion persists judge and strategy, not fallback', () => {
    const combo = normalizeCombo({name:'quality', strategy:'fusion', models:['a','b'], 'judge-model':'provider::j', 'min-successful-models':2});
    expect(combo.strategy).toBe('fusion');
    expect(combo['judge-model']).toBe('provider::j');
    expect(comboValidation(combo)).toBe('');
    expect(comboValidation({...combo, 'judge-model':''})).toBe('judgeRequired');
    expect(comboValidation({...combo, 'min-successful-models':3})).toBe('minimumInvalid');
  });
  test('empty, self referencing and unknown strategy are rejected', () => {
    expect(comboValidation(emptyCombo())).toBe('required');
    expect(comboValidation({...emptyCombo(), name:'a', models:['a']})).toBe('nested');
    expect(comboValidation({...emptyCombo(), name:'a', models:['b'], strategy:'typo'})).toBe('strategy');
  });
  test('every supported locale supplies all labels', () => {
    const keys = Object.keys(comboLocale('en')).sort();
    for (const language of ['zh-CN','zh-TW','ru','vi']) {
      expect(Object.keys(comboLocale(language)).sort()).toEqual(keys);
      expect(Object.values(comboLocale(language)).every(value => typeof value === 'string' && value.length > 0)).toBe(true);
    }
  });
});
