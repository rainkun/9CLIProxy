import { addMember, comboValidation, emptyCombo, filterCatalog, moveMember, normalizeCombo, splitMember } from './model-options.js';
import { comboLocale } from './locales.js';

// The vendored management bundle supplies its existing React, authenticated API
// client, stores and i18n hook. This page never creates a second React runtime.
export function createCombosPage(React, api, useConnection, useNotifications, useTranslation) {
  const { useState, useEffect, useMemo, useCallback } = React;

  return function CombosPage() {
    const { i18n } = useTranslation();
    const text = comboLocale(i18n?.resolvedLanguage || i18n?.language);
    const connection = useConnection(state => state.connectionStatus);
    const notify = useNotifications(state => state.showNotification);
    const confirm = useNotifications(state => state.showConfirmation);
    const [combos, setCombos] = useState([]);
    const [providers, setProviders] = useState([]);
    const [draft, setDraft] = useState(emptyCombo);
    const [editing, setEditing] = useState('');
    const [provider, setProvider] = useState('');
    const [query, setQuery] = useState('');
    const [judgeProvider, setJudgeProvider] = useState('');
    const [custom, setCustom] = useState('');
    const [loading, setLoading] = useState(false);
    const [saving, setSaving] = useState(false);
    const [error, setError] = useState('');
    const [catalogError, setCatalogError] = useState('');
    const blocked = connection !== 'connected' || saving;
    const fusion = draft.strategy === 'fusion';
    const strategyLabel = strategy => text[{ fallback: 'fallback', 'round-robin': 'roundRobin', fusion: 'fusion' }[strategy]] || strategy;
    const providerLabel = id => providers.find(provider => provider.id === id)?.name || id || 'Auto';
    const update = (key, value) => setDraft(current => ({ ...current, [key]: value }));
    const allModels = useMemo(() => filterCatalog(providers, '', ''), [providers]);
    const modelLookup = useMemo(() => new Map(allModels.map(model => [model.value, model])), [allModels]);
    const available = useMemo(() => filterCatalog(providers, provider, query), [providers, provider, query]);
    const judgeModels = useMemo(() => filterCatalog(providers, judgeProvider, ''), [providers, judgeProvider]);

    const reload = useCallback(async () => {
      setLoading(true);
      setError('');
      setCatalogError('');
      const [saved, catalog] = await Promise.allSettled([api.get('/combos'), api.get('/combos/models')]);
      if (saved.status === 'fulfilled') {
        setCombos((Array.isArray(saved.value) ? saved.value : saved.value.combos || []).map(normalizeCombo));
      } else {
        setError(saved.reason?.message || 'Unable to load combos');
      }
      if (catalog.status === 'fulfilled') {
        setProviders(Array.isArray(catalog.value.providers) ? catalog.value.providers : []);
      } else {
        setCatalogError(catalog.reason?.message || 'Unable to load model registry');
      }
      setLoading(false);
    }, []);

    useEffect(() => {
      if (connection === 'connected') void reload();
    }, [connection, reload]);

    function reset() {
      setDraft(emptyCombo());
      setEditing('');
      setJudgeProvider('');
      setCustom('');
      setError('');
    }

    function edit(combo) {
      setDraft(normalizeCombo(combo));
      setEditing(combo.name);
      setJudgeProvider(splitMember(combo['judge-model'] || '').provider);
      setError('');
      document.getElementById('combo-editor')?.scrollIntoView({ behavior: 'smooth', block: 'start' });
    }

    async function save() {
      const payload = normalizeCombo(draft);
      const validation = comboValidation(payload);
      if (validation) {
        setError(text[validation] || text.required);
        return;
      }
      if (!editing && combos.some(combo => combo.name.toLowerCase() === payload.name.toLowerCase())) {
        setError(text.duplicateName);
        return;
      }
      setSaving(true);
      setError('');
      try {
        await api.patch('/combos', payload);
        await reload();
        reset();
        notify(text.saved, 'success');
      } catch (failure) {
        setError(failure.message || text.error);
      } finally {
        setSaving(false);
      }
    }

    function removeCombo(combo) {
      confirm({
        title: text.deleteTitle,
        message: `${combo.name} — ${text.deleteMessage}`,
        confirmText: text.delete,
        onConfirm: async () => {
          setSaving(true);
          try {
            await api.delete(`/combos?name=${encodeURIComponent(combo.name)}`);
            if (editing === combo.name) reset();
            await reload();
            notify(text.deleted, 'success');
          } catch (failure) {
            setError(failure.message || text.error);
          } finally {
            setSaving(false);
          }
        },
      });
    }

    return (
      <div className="cp-combos">
        <header className="cp-combos-header">
          <div><span className="cp-combos-eyebrow">{text.strategy}</span><h1>{text.title}</h1><p>{text.subtitle}</p></div>
          <button className="cp-button" onClick={() => void reload()} disabled={blocked || loading}>{loading ? text.loading : text.refresh}</button>
        </header>
        {connection !== 'connected' && <p className="cp-notice" role="status">{text.offline}</p>}
        <div className="cp-combos-layout">
          <section className="cp-panel" id="combo-editor" aria-label={editing ? text.edit : text.newCombo}>
            <div className="cp-section-head"><h2>{editing ? text.edit : text.newCombo}</h2><span className="cp-badge">{draft.models.length} {text.models}</span></div>
            <div className="cp-fields">
              <label htmlFor="combo-name">{text.name}<input id="combo-name" value={draft.name} placeholder="my-coding-combo" onChange={event => update('name', event.target.value)} disabled={blocked || !!editing} /><small>{text.nameHint}</small></label>
              <label htmlFor="combo-display-name">{text.displayName}<input id="combo-display-name" value={draft['display-name']} onChange={event => update('display-name', event.target.value)} disabled={blocked} /></label>
            </div>
            <fieldset className="cp-strategies"><legend>{text.strategy}</legend>
              {['fallback', 'round-robin', 'fusion'].map((strategy, index) => (
                <button type="button" key={strategy} aria-pressed={draft.strategy === strategy} disabled={blocked}
                  className={`cp-strategy ${draft.strategy === strategy ? 'active' : ''}`} onClick={() => update('strategy', strategy)}>
                  <span className="cp-strategy-symbol" aria-hidden="true">{['↓', '↻', '✦'][index]}</span>
                  <strong>{strategyLabel(strategy)}</strong>
                  <span>{text[['fallbackHint', 'roundRobinHint', 'fusionHint'][index]]}</span>
                </button>
              ))}
            </fieldset>
            {draft.strategy === 'round-robin' && <label className="cp-sticky" htmlFor="combo-sticky">{text.sticky}
              <input id="combo-sticky" type="number" min="1" step="1" value={draft['sticky-round-robin-limit']} disabled={blocked}
                onChange={event => update('sticky-round-robin-limit', Number(event.target.value))} /><small>{text.stickyHint}</small></label>}
            <div className="cp-model-workspace">
              <section aria-label={text.catalog}>
                <div className="cp-section-head"><h3>{text.catalog}</h3><span className="cp-count">{available.length}</span></div>
                <small>{text.catalogHint}</small>
                <label htmlFor="combo-provider">{text.provider}<select id="combo-provider" value={provider} onChange={event => setProvider(event.target.value)} disabled={blocked || loading}>
                  <option value="">{text.allProviders}</option>{providers.map(group => <option key={group.id} value={group.id}>{group.name || group.id} ({group.models.length})</option>)}
                </select></label>
                <input type="search" aria-label={text.search} placeholder={text.search} value={query} onChange={event => setQuery(event.target.value)} disabled={blocked} />
                {catalogError && <p className="cp-error" role="alert">{catalogError}</p>}
                <div className="cp-catalog" aria-busy={loading}>
                  {loading ? <p className="cp-empty">{text.loading}</p> : !providers.length ? <p className="cp-empty">{text.noProviders}</p> : !available.length ? <p className="cp-empty">{text.noModels}</p> :
                    available.slice(0, 200).map(model => {
                      const selected = draft.models.some(value => value.toLowerCase() === model.value.toLowerCase());
                      return <div className="cp-model-row" key={model.value}>
                        <div><span className="cp-provider-tag">{model.providerName}</span><strong title={model.id}>{model.id}</strong>{model.name !== model.id && <small>{model.name}</small>}</div>
                        <button type="button" className="cp-add" aria-label={`${text.add} ${model.id} (${model.providerName})`}
                          disabled={blocked || selected || (fusion && draft.models.length >= 16)}
                          onClick={() => update('models', addMember(draft.models, model.value))}>{selected ? '✓' : '+'}<span className="cp-sr-only">{selected ? text.added : text.add}</span></button>
                      </div>;
                    })}
                  {available.length > 200 && <small className="cp-empty">200 / {available.length} · {text.search}</small>}
                </div>
              </section>
              <section aria-label={text.selected}>
                <div className="cp-section-head"><h3>{text.selected}</h3><span className="cp-count">{draft.models.length}</span></div>
                <small>{fusion ? text.panelHint : text.selectedHint}</small>
                <ol className="cp-selected">
                  {!draft.models.length && <li className="cp-empty">{text.selectHint}</li>}
                  {draft.models.map((value, index) => {
                    const member = splitMember(value);
                    const known = modelLookup.has(value) || (!member.provider && allModels.some(model => model.id === value));
                    return <li className="cp-selected-row" key={value}>
                      <span className="cp-order">{index + 1}</span>
                      <div className="cp-selected-model"><span className="cp-provider-tag">{providerLabel(member.provider)}</span><strong title={member.model}>{member.model}</strong>
                        {!known && !loading && <small className="cp-warning">{text.unavailable}</small>}</div>
                      <div className="cp-row-actions">
                        <button className="cp-icon-button" aria-label={`${text.up} ${member.model}`} disabled={blocked || index === 0} onClick={() => update('models', moveMember(draft.models, index, -1))}>↑</button>
                        <button className="cp-icon-button" aria-label={`${text.down} ${member.model}`} disabled={blocked || index === draft.models.length - 1} onClick={() => update('models', moveMember(draft.models, index, 1))}>↓</button>
                        <button className="cp-icon-button" aria-label={`${text.remove} ${member.model}`} disabled={blocked} onClick={() => update('models', draft.models.filter((_, i) => i !== index))}>×</button>
                      </div>
                    </li>;
                  })}
                </ol>
              </section>
            </div>
            {fusion && <section className="cp-fusion" aria-label={text.judge}>
              <div className="cp-section-head"><div><h3>✦ {text.judge}</h3><small>{text.judgeHint}</small></div>
                <span className="cp-cost">{draft.models.length} + 1 <small>{text.cost}</small></span></div>
              <div className="cp-fields">
                <label htmlFor="combo-judge-provider">{text.provider}<select id="combo-judge-provider" value={judgeProvider} disabled={blocked} onChange={event => {setJudgeProvider(event.target.value); update('judge-model', '');}}>
                  <option value="">{text.allProviders}</option>{providers.map(group => <option value={group.id} key={group.id}>{group.name || group.id}</option>)}
                </select></label>
                <label htmlFor="combo-judge-model">{text.judge}<select id="combo-judge-model" value={draft['judge-model']} disabled={blocked} onChange={event => update('judge-model', event.target.value)}>
                  <option value="">{text.chooseModel}</option>
                  {draft['judge-model'] && !judgeModels.some(model => model.value === draft['judge-model']) && <option value={draft['judge-model']}>{draft['judge-model']} · {text.unavailable}</option>}
                  {judgeModels.map(model => <option value={model.value} key={model.value}>{model.providerName} · {model.id}</option>)}
                </select></label>
              </div>
              <p className="cp-warning">{text.costHint}</p><small>{text.fusionScope}</small><small>{text.usageHint}</small>
            </section>}
            <details className="cp-advanced"><summary>{text.advanced}</summary>
              <label htmlFor="combo-custom">{text.custom}<div className="cp-inline"><input id="combo-custom" value={custom} placeholder="provider::model" disabled={blocked} onChange={event => setCustom(event.target.value)} />
                <button className="cp-button" disabled={blocked || !custom.trim() || (fusion && draft.models.length >= 16)}
                  onClick={() => {update('models', addMember(draft.models, custom)); setCustom('');}}>{text.add}</button></div><small>{text.customHint}</small></label>
              {fusion && <div className="cp-fields">
                <label htmlFor="combo-custom-judge">{text.customJudge}<input id="combo-custom-judge" value={draft['judge-model']} disabled={blocked} onChange={event => update('judge-model', event.target.value)} /></label>
                <label htmlFor="combo-minimum">{text.minimum}<input id="combo-minimum" type="number" min="1" max={Math.max(1, draft.models.length)} step="1" value={draft['min-successful-models']} disabled={blocked} onChange={event => update('min-successful-models', Number(event.target.value))} /><small>{text.minimumHint}</small></label>
                <label className="cp-full" htmlFor="combo-judge-prompt">{text.prompt}<textarea id="combo-judge-prompt" rows="3" value={draft['judge-prompt']} disabled={blocked} onChange={event => update('judge-prompt', event.target.value)} /></label>
              </div>}
            </details>
            {error && <p className="cp-error" role="alert">{error}</p>}
            <footer className="cp-editor-footer">
              <label className="cp-checkbox"><input type="checkbox" checked={draft.disabled} disabled={blocked} onChange={event => update('disabled', event.target.checked)} />{text.disable}</label>
              <div className="cp-inline"><button className="cp-button" onClick={reset} disabled={saving}>{text.cancel}</button>
                <button className="cp-button cp-primary" onClick={() => void save()} disabled={blocked || loading}>{saving ? text.saving : text.save}</button></div>
            </footer>
          </section>
          <aside className="cp-saved" aria-label={text.savedCombos}>
            <div className="cp-section-head"><h2>{text.savedCombos}</h2><span className="cp-count">{combos.length}</span></div>
            {combos.length === 0 && <div className="cp-panel cp-empty"><strong>{loading ? text.loading : text.emptySaved}</strong><p>{text.emptySavedHint}</p></div>}
            {combos.map(combo => <article className={`cp-panel cp-saved-card ${combo.disabled ? 'is-disabled' : ''} ${editing === combo.name ? 'is-editing' : ''}`} key={combo.name}>
              <div className="cp-section-head"><strong>{combo['display-name'] || combo.name}</strong><span className="cp-badge">{strategyLabel(combo.strategy)}</span></div>
              <code>{combo.name}</code>
              <ol>{combo.models.map((model, index) => <li key={model}><span className="cp-order">{index + 1}</span><span>{splitMember(model).model}<small>{providerLabel(splitMember(model).provider)}</small></span></li>)}</ol>
              {combo.strategy === 'fusion' && <p className="cp-saved-judge">✦ {splitMember(combo['judge-model']).model}<small>{combo.models.length} + 1 · {text.cost}</small></p>}
              <div className="cp-saved-footer"><span className={combo.disabled ? '' : 'cp-status'}>{combo.disabled ? text.disabled : text.enabled}</span>
                <div className="cp-inline"><button className="cp-button" disabled={blocked} onClick={() => edit(combo)}>{text.edit}</button><button className="cp-button cp-danger" disabled={blocked} onClick={() => removeCombo(combo)}>{text.delete}</button></div></div>
            </article>)}
          </aside>
        </div>
      </div>
    );
  };
}
