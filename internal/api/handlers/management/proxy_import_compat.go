package management

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

// nineRouterBackup is the subset of a 9router database export that can be
// represented by CLIProxyAPI's file/config model. Proxy pools and API keys are
// intentionally not imported as independent runtime objects.
type nineRouterBackup struct {
	Connections   []map[string]any
	ProviderNodes []map[string]any
	CustomModels  []map[string]any
	ModelAliases  map[string]string
}

type nineRouterCompatNode struct {
	ID      string
	Type    string
	Name    string
	Prefix  string
	APIType string
	BaseURL string
}

type pendingOpenAICompatImport struct {
	recordIndex int
	record      map[string]any
	node        nineRouterCompatNode
	apiKey      string
	proxyURL    string
	configName  string
}

type openAICompatImportGroup struct {
	node    nineRouterCompatNode
	pending []pendingOpenAICompatImport
}

// decodeNineRouterBackup parses both the current 9router export shape and the
// older single-credential/array forms accepted by the auth uploader.
func decodeNineRouterBackup(data []byte) (*nineRouterBackup, bool, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()

	var root any
	if errDecode := decoder.Decode(&root); errDecode != nil {
		return nil, false, fmt.Errorf("invalid JSON: %w", errDecode)
	}

	out := &nineRouterBackup{
		Connections:   make([]map[string]any, 0),
		ProviderNodes: make([]map[string]any, 0),
		CustomModels:  make([]map[string]any, 0),
		ModelAliases:  make(map[string]string),
	}

	if array, ok := root.([]any); ok {
		out.Connections = mapsFromAny(array)
		return out, len(out.Connections) > 0, nil
	}

	object, ok := root.(map[string]any)
	if !ok {
		return out, false, nil
	}

	recognized := false
	for _, key := range []string{"providerConnections", "provider_connections", "accounts", "connections"} {
		if raw, exists := object[key]; exists {
			recognized = true
			if array, okArray := raw.([]any); okArray {
				out.Connections = mapsFromAny(array)
			}
			break
		}
	}
	for _, key := range []string{"providerNodes", "provider_nodes", "nodes"} {
		if raw, exists := object[key]; exists {
			recognized = true
			if array, okArray := raw.([]any); okArray {
				out.ProviderNodes = mapsFromAny(array)
			}
			break
		}
	}
	for _, key := range []string{"customModels", "custom_models"} {
		if raw, exists := object[key]; exists {
			recognized = true
			if array, okArray := raw.([]any); okArray {
				out.CustomModels = mapsFromAny(array)
			}
			break
		}
	}
	for _, key := range []string{"modelAliases", "model_aliases", "aliases"} {
		if raw, exists := object[key]; exists {
			recognized = true
			out.ModelAliases = stringMapFromAny(raw)
			break
		}
	}

	// A raw credential object is still accepted by the uploader.
	if _, hasProvider := object["provider"]; hasProvider {
		out.Connections = []map[string]any{object}
		recognized = true
	} else if _, hasAccessToken := object["accessToken"]; hasAccessToken {
		out.Connections = []map[string]any{object}
		recognized = true
	} else if _, hasAccessToken := object["access_token"]; hasAccessToken {
		out.Connections = []map[string]any{object}
		recognized = true
	}

	return out, recognized, nil
}

func stringMapFromAny(raw any) map[string]string {
	out := make(map[string]string)
	switch typed := raw.(type) {
	case map[string]string:
		for key, value := range typed {
			if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
				out[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
	case map[string]any:
		for key, value := range typed {
			if strings.TrimSpace(key) == "" || value == nil {
				continue
			}
			switch item := value.(type) {
			case string:
				if strings.TrimSpace(item) != "" {
					out[strings.TrimSpace(key)] = strings.TrimSpace(item)
				}
			case map[string]any:
				if target := importStringValue(item, "model", "target", "value", "fullModel", "full_model"); target != "" {
					out[strings.TrimSpace(key)] = strings.TrimSpace(target)
				} else if provider := importStringValue(item, "provider", "providerAlias", "provider_alias"); provider != "" {
					if model := importStringValue(item, "modelId", "model_id", "modelName", "model_name"); model != "" {
						out[strings.TrimSpace(key)] = strings.TrimSpace(provider) + "/" + strings.TrimSpace(model)
					}
				}
			default:
				text := strings.TrimSpace(fmt.Sprint(item))
				if text != "" {
					out[strings.TrimSpace(key)] = text
				}
			}
		}
	}
	return out
}

func nineRouterCompatNodes(raw []map[string]any) []nineRouterCompatNode {
	out := make([]nineRouterCompatNode, 0, len(raw))
	for _, record := range raw {
		if record == nil {
			continue
		}
		node := nineRouterCompatNode{
			ID:      strings.TrimSpace(importStringValue(record, "id", "provider", "providerId", "provider_id")),
			Type:    strings.ToLower(strings.TrimSpace(importStringValue(record, "type", "nodeType", "node_type"))),
			Name:    strings.TrimSpace(importStringValue(record, "name", "nodeName", "node_name", "displayName", "display_name")),
			Prefix:  strings.Trim(strings.TrimSpace(importStringValue(record, "prefix")), "/"),
			APIType: strings.ToLower(strings.TrimSpace(importStringValue(record, "apiType", "api_type"))),
			BaseURL: strings.TrimSpace(importStringValue(record, "baseUrl", "base_url", "baseURL")),
		}
		if node.ID == "" {
			continue
		}
		if node.APIType == "" {
			if strings.Contains(strings.ToLower(node.ID), "responses") {
				node.APIType = "responses"
			} else {
				node.APIType = "chat"
			}
		}
		node.Type = strings.ReplaceAll(node.Type, "_", "-")
		node.BaseURL = normalizeImportedCompatBaseURL(node.BaseURL)
		if node.Name == "" {
			node.Name = node.ID
		}
		out = append(out, node)
	}
	return out
}

func normalizeImportedCompatBaseURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	value = strings.TrimRight(value, "/")
	for _, suffix := range []string{"/chat/completions", "/responses"} {
		if strings.HasSuffix(strings.ToLower(value), suffix) {
			value = strings.TrimRight(value[:len(value)-len(suffix)], "/")
			break
		}
	}
	return value
}

func isNineRouterOpenAICompatNode(node nineRouterCompatNode) bool {
	kind := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(node.Type)), "_", "-")
	return kind == "openai-compatible" || strings.HasPrefix(strings.ToLower(node.ID), "openai-compatible-")
}

func isNineRouterAnthropicCompatNode(node nineRouterCompatNode) bool {
	kind := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(node.Type)), "_", "-")
	return kind == "anthropic-compatible" || strings.HasPrefix(strings.ToLower(node.ID), "anthropic-compatible-")
}

func indexNineRouterCompatNodes(nodes []nineRouterCompatNode) map[string]nineRouterCompatNode {
	out := make(map[string]nineRouterCompatNode, len(nodes)*3)
	// IDs are the strongest identity and win over human aliases.
	for _, node := range nodes {
		if key := strings.ToLower(strings.TrimSpace(node.ID)); key != "" {
			out[key] = node
		}
	}
	for _, node := range nodes {
		for _, raw := range []string{node.Prefix, node.Name} {
			key := strings.ToLower(strings.TrimSpace(raw))
			if key == "" {
				continue
			}
			if _, exists := out[key]; !exists {
				out[key] = node
			}
		}
	}
	return out
}

func isNativeNineRouterProviderID(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "openai", "anthropic", "claude", "google", "gemini", "xai", "x-ai", "codex", "chatgpt", "vertex", "kimi":
		return true
	default:
		return false
	}
}

func rawImportedProviderID(record map[string]any) string {
	return strings.ToLower(strings.TrimSpace(importStringValue(record, "provider", "provider_id", "providerId", "type")))
}

func importedConnectionProxyURL(record map[string]any) (string, string) {
	if record == nil {
		return "", ""
	}
	if value := strings.TrimSpace(importStringValue(record, "proxyUrl", "proxy_url", "connectionProxyUrl", "connection_proxy_url")); value != "" {
		return value, ""
	}
	nested := importedProviderSpecificData(record)
	if nested == nil {
		return "", ""
	}
	enabled := true
	if rawEnabled, ok := nested["connectionProxyEnabled"]; ok {
		if parsed, okParsed := importedBoolValue(rawEnabled); okParsed {
			enabled = parsed
		}
	}
	poolID := strings.TrimSpace(importStringValue(nested, "connectionProxyPoolId", "connection_proxy_pool_id", "proxyPoolId", "proxy_pool_id"))
	if !enabled {
		return "", poolID
	}
	return strings.TrimSpace(importStringValue(nested, "connectionProxyUrl", "connection_proxy_url", "proxyUrl", "proxy_url")), poolID
}

func importedOpenAICompatConfigName(node nineRouterCompatNode) string {
	name := strings.TrimSpace(node.Name)
	if name == "" {
		name = strings.TrimSpace(node.ID)
	}
	if name == "" {
		return "openai-compatibility"
	}
	return name
}

func importedOpenAICompatEntryIdentity(entry config.OpenAICompatibility) string {
	return strings.ToLower(strings.TrimSpace(entry.Name)) + "|" +
		strings.TrimRight(strings.ToLower(strings.TrimSpace(entry.BaseURL)), "/") + "|" +
		strings.ToLower(strings.Trim(strings.TrimSpace(entry.Prefix), "/"))
}

func importedOpenAICompatAPIKeyIdentity(entry config.OpenAICompatibilityAPIKey) string {
	return strings.TrimSpace(entry.APIKey) + "|" + strings.TrimSpace(entry.ProxyURL)
}

func cloneImportedOpenAICompatEntry(in config.OpenAICompatibility) config.OpenAICompatibility {
	out := in
	out.APIKeyEntries = append([]config.OpenAICompatibilityAPIKey(nil), in.APIKeyEntries...)
	out.Models = append([]config.OpenAICompatibilityModel(nil), in.Models...)
	if in.Headers != nil {
		out.Headers = make(map[string]string, len(in.Headers))
		for key, value := range in.Headers {
			out.Headers[key] = value
		}
	}
	return out
}

func mergeImportedOpenAICompatEntries(existing, incoming []config.OpenAICompatibility) []config.OpenAICompatibility {
	out := make([]config.OpenAICompatibility, 0, len(existing)+len(incoming))
	index := make(map[string]int, len(existing)+len(incoming))
	for _, entry := range existing {
		copyEntry := cloneImportedOpenAICompatEntry(entry)
		key := importedOpenAICompatEntryIdentity(copyEntry)
		if key == "||" {
			continue
		}
		if previous, ok := index[key]; ok {
			out[previous] = mergeOneImportedOpenAICompatEntry(out[previous], copyEntry)
			continue
		}
		index[key] = len(out)
		out = append(out, copyEntry)
	}
	for _, entry := range incoming {
		copyEntry := cloneImportedOpenAICompatEntry(entry)
		key := importedOpenAICompatEntryIdentity(copyEntry)
		if key == "||" {
			continue
		}
		if previous, ok := index[key]; ok {
			out[previous] = mergeOneImportedOpenAICompatEntry(out[previous], copyEntry)
			continue
		}
		index[key] = len(out)
		out = append(out, copyEntry)
	}
	return out
}

func mergeOneImportedOpenAICompatEntry(dst, src config.OpenAICompatibility) config.OpenAICompatibility {
	if dst.BaseURL == "" {
		dst.BaseURL = src.BaseURL
	}
	if dst.Prefix == "" {
		dst.Prefix = src.Prefix
	}
	if dst.Priority == 0 {
		dst.Priority = src.Priority
	}
	if len(dst.Headers) == 0 && len(src.Headers) > 0 {
		dst.Headers = cloneImportedOpenAICompatEntry(src).Headers
	}
	keySet := make(map[string]struct{}, len(dst.APIKeyEntries))
	for _, key := range dst.APIKeyEntries {
		keySet[importedOpenAICompatAPIKeyIdentity(key)] = struct{}{}
	}
	for _, key := range src.APIKeyEntries {
		identity := importedOpenAICompatAPIKeyIdentity(key)
		if identity == "|" {
			continue
		}
		if _, exists := keySet[identity]; exists {
			continue
		}
		keySet[identity] = struct{}{}
		dst.APIKeyEntries = append(dst.APIKeyEntries, key)
	}
	modelSet := make(map[string]struct{}, len(dst.Models))
	for _, model := range dst.Models {
		modelSet[strings.ToLower(strings.TrimSpace(model.Name))+"|"+strings.ToLower(strings.TrimSpace(model.Alias))] = struct{}{}
	}
	for _, model := range src.Models {
		identity := strings.ToLower(strings.TrimSpace(model.Name)) + "|" + strings.ToLower(strings.TrimSpace(model.Alias))
		if identity == "|" {
			continue
		}
		if _, exists := modelSet[identity]; exists {
			continue
		}
		modelSet[identity] = struct{}{}
		dst.Models = append(dst.Models, model)
	}
	return dst
}

// planImportedOpenAICompat builds config entries and marks compatible
// connection records as handled. Unsupported nodes are deliberately skipped
// instead of being represented by a misleading native provider.
func planImportedOpenAICompat(backup *nineRouterBackup, result *nineRouterImportResult) ([]config.OpenAICompatibility, []pendingOpenAICompatImport, map[int]struct{}) {
	entries := make([]config.OpenAICompatibility, 0)
	pending := make([]pendingOpenAICompatImport, 0)
	handled := make(map[int]struct{})
	if backup == nil {
		return entries, pending, handled
	}

	nodes := nineRouterCompatNodes(backup.ProviderNodes)
	nodeByAlias := indexNineRouterCompatNodes(nodes)
	groups := make(map[string]*openAICompatImportGroup)
	groupOrder := make([]string, 0)

	for index, record := range backup.Connections {
		if record == nil {
			continue
		}
		rawProvider := rawImportedProviderID(record)
		node, matched := nodeByAlias[strings.ToLower(strings.TrimSpace(rawProvider))]
		// Native provider IDs such as "openai" and "claude" are also valid
		// human node names. Do not let a name/prefix alias capture those
		// connections; only a concrete node ID or explicit nested node metadata
		// may identify a compatible connection.
		if matched && strings.ToLower(strings.TrimSpace(node.ID)) != strings.ToLower(strings.TrimSpace(rawProvider)) &&
			isNativeNineRouterProviderID(rawProvider) {
			matched = false
		}
		if !matched {
			if nested := importedProviderSpecificData(record); nested != nil {
				aliases := []string{"provider", "providerId", "provider_id", "nodeName", "node_name"}
				if !isNativeNineRouterProviderID(rawProvider) {
					aliases = append(aliases, "prefix")
				}
				for _, aliasKey := range aliases {
					alias := importStringValue(nested, aliasKey)
					if strings.TrimSpace(alias) == "" {
						continue
					}
					if candidate, ok := nodeByAlias[strings.ToLower(strings.TrimSpace(alias))]; ok {
						node = candidate
						matched = true
						break
					}
				}
			}
		}
		if !matched {
			if strings.HasPrefix(rawProvider, "openai-compatible-") || strings.HasPrefix(rawProvider, "anthropic-compatible-") {
				handled[index] = struct{}{}
				result.Skipped = append(result.Skipped, map[string]any{
					"index": index, "provider": rawProvider,
					"reason": "provider node definition is missing from 9router backup",
				})
			}
			continue
		}
		if !isNineRouterOpenAICompatNode(node) && !isNineRouterAnthropicCompatNode(node) {
			continue
		}
		handled[index] = struct{}{}

		if isNineRouterAnthropicCompatNode(node) {
			result.Skipped = append(result.Skipped, map[string]any{
				"index": index, "provider": node.ID,
				"reason": "anthropic-compatible nodes are not imported because CLIProxyAPI has no native Anthropic compatibility config",
			})
			continue
		}
		if strings.EqualFold(strings.TrimSpace(node.APIType), "responses") {
			result.Skipped = append(result.Skipped, map[string]any{
				"index": index, "provider": node.ID,
				"reason": "OpenAI Responses nodes are not imported as chat compatibility entries",
			})
			continue
		}
		if importedRecordDisabled(record) {
			result.Skipped = append(result.Skipped, map[string]any{
				"index": index, "provider": node.ID,
				"reason": "connection is inactive",
			})
			continue
		}
		apiKey := importedAPIKeyValue(record)
		if apiKey == "" {
			result.Skipped = append(result.Skipped, map[string]any{
				"index": index, "provider": node.ID,
				"reason": "OpenAI-compatible connection API key is missing",
			})
			continue
		}
		if node.BaseURL == "" {
			result.Failed = append(result.Failed, map[string]any{
				"index": index, "provider": node.ID,
				"error": "OpenAI-compatible provider node base URL is missing",
			})
			continue
		}
		proxyURL, poolID := importedConnectionProxyURL(record)
		if proxyURL != "" {
			if _, errParse := proxyutil.Parse(proxyURL); errParse != nil {
				result.Failed = append(result.Failed, map[string]any{
					"index": index, "provider": node.ID,
					"error": fmt.Sprintf("invalid OpenAI-compatible proxy URL: %v", errParse),
				})
				continue
			}
		}
		if poolID != "" {
			result.Skipped = append(result.Skipped, map[string]any{
				"index": index, "provider": node.ID, "proxy_pool_id": poolID,
				"reason": "proxy pool reference was not imported; configure a concrete proxy URL manually",
			})
		}

		groupKey := strings.ToLower(node.ID)
		if _, exists := groups[groupKey]; !exists {
			groups[groupKey] = &openAICompatImportGroup{node: node}
			groupOrder = append(groupOrder, groupKey)
		}
		groups[groupKey].pending = append(groups[groupKey].pending, pendingOpenAICompatImport{
			recordIndex: index,
			record:      record,
			node:        node,
			apiKey:      apiKey,
			proxyURL:    proxyURL,
			configName:  importedOpenAICompatConfigName(node),
		})
	}

	for _, groupKey := range groupOrder {
		group := groups[groupKey]
		if group == nil || len(group.pending) == 0 {
			continue
		}
		entry := config.OpenAICompatibility{
			Name:    importedOpenAICompatConfigName(group.node),
			Prefix:  strings.Trim(group.node.Prefix, "/"),
			BaseURL: group.node.BaseURL,
			Models:  importedOpenAICompatModels(group.node, backup.CustomModels, backup.ModelAliases, group.pending),
		}
		seenKeys := make(map[string]struct{})
		for _, item := range group.pending {
			nested := importedProviderSpecificData(item.record)
			if entry.Priority == 0 {
				entry.Priority = importedFirstInt(item.record, nested, []string{"priority"}, []string{"priority"})
			}
			if len(entry.Headers) == 0 {
				entry.Headers = importedFirstStringMap(item.record, nested, []string{"headers"}, []string{"headers"})
			}
			weight := importedFirstOptionalInt(item.record, nested, []string{"weight"}, []string{"weight"})
			if errWeight := config.ValidateCredentialWeight(weight); errWeight != nil {
				result.Failed = append(result.Failed, map[string]any{
					"index": item.recordIndex, "provider": group.node.ID,
					"error": fmt.Sprintf("invalid OpenAI-compatible weight: %v", errWeight),
				})
				continue
			}
			keyEntry := config.OpenAICompatibilityAPIKey{
				APIKey:   item.apiKey,
				Weight:   weight,
				ProxyURL: item.proxyURL,
			}
			keyIdentity := importedOpenAICompatAPIKeyIdentity(keyEntry)
			if _, exists := seenKeys[keyIdentity]; exists {
				continue
			}
			seenKeys[keyIdentity] = struct{}{}
			entry.APIKeyEntries = append(entry.APIKeyEntries, keyEntry)
			// Keep one pending item per unique key. The config entry is
			// deduplicated by API key + proxy URL, so emitting duplicate
			// summaries for repeated backup rows would be misleading.
			pending = append(pending, item)
		}
		if len(entry.APIKeyEntries) == 0 {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, pending, handled
}

func importedOpenAICompatModels(node nineRouterCompatNode, customModels []map[string]any, aliases map[string]string, pending []pendingOpenAICompatImport) []config.OpenAICompatibilityModel {
	nodeAliases := map[string]struct{}{}
	for _, raw := range []string{node.ID, node.Prefix, node.Name} {
		if value := strings.ToLower(strings.TrimSpace(raw)); value != "" {
			nodeAliases[value] = struct{}{}
		}
	}
	models := make([]config.OpenAICompatibilityModel, 0)
	seen := make(map[string]int)
	appendModel := func(model config.OpenAICompatibilityModel) {
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" {
			return
		}
		if model.Alias == "" {
			model.Alias = model.Name
		}
		key := strings.ToLower(model.Name) + "|" + strings.ToLower(model.Alias)
		if previous, exists := seen[key]; exists {
			if models[previous].DisplayName == "" {
				models[previous].DisplayName = model.DisplayName
			}
			if model.Image {
				models[previous].Image = true
			}
			return
		}
		seen[key] = len(models)
		models = append(models, model)
	}

	for _, raw := range customModels {
		if raw == nil {
			continue
		}
		providerAlias := strings.ToLower(strings.TrimSpace(importStringValue(raw, "providerAlias", "provider_alias", "provider", "providerId", "provider_id")))
		if _, matches := nodeAliases[providerAlias]; !matches {
			continue
		}
		modelID := importStringValue(raw, "id", "model", "modelId", "model_id")
		modelID = stripImportedCompatProviderPrefix(modelID, node)
		if modelID == "" {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(importStringValue(raw, "type", "kind")))
		switch kind {
		case "", "llm", "imagetotext", "image-to-text":
			appendModel(config.OpenAICompatibilityModel{
				Name:        modelID,
				Alias:       modelID,
				DisplayName: strings.TrimSpace(importStringValue(raw, "name", "displayName", "display_name")),
			})
		case "image":
			appendModel(config.OpenAICompatibilityModel{
				Name:        modelID,
				Alias:       modelID,
				DisplayName: strings.TrimSpace(importStringValue(raw, "name", "displayName", "display_name")),
				Image:       true,
			})
		}
	}

	for alias, target := range aliases {
		targetProvider, targetModel, ok := strings.Cut(strings.TrimSpace(target), "/")
		if !ok || strings.TrimSpace(targetModel) == "" {
			continue
		}
		if _, matches := nodeAliases[strings.ToLower(strings.TrimSpace(targetProvider))]; !matches {
			continue
		}
		alias = stripImportedCompatProviderPrefix(alias, node)
		targetModel = stripImportedCompatProviderPrefix(targetModel, node)
		if strings.TrimSpace(alias) == "" || strings.TrimSpace(targetModel) == "" {
			continue
		}
		appendModel(config.OpenAICompatibilityModel{
			Name:  targetModel,
			Alias: alias,
		})
	}

	// 9router stores the currently selected model in the connection record even
	// when customModels is empty. Keep those explicit models routable.
	for _, item := range pending {
		for _, key := range []string{"defaultModel", "default_model"} {
			if modelID := stripImportedCompatProviderPrefix(importStringValue(item.record, key), node); modelID != "" {
				appendModel(config.OpenAICompatibilityModel{Name: modelID, Alias: modelID})
			}
		}
		for key := range item.record {
			if strings.HasPrefix(strings.ToLower(key), "modellock_") {
				modelID := stripImportedCompatProviderPrefix(key[len("modelLock_"):], node)
				if modelID != "" {
					appendModel(config.OpenAICompatibilityModel{Name: modelID, Alias: modelID})
				}
			}
		}
	}
	return models
}

func stripImportedCompatProviderPrefix(raw string, node nineRouterCompatNode) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	for _, prefix := range []string{node.Prefix, node.ID, node.Name} {
		prefix = strings.Trim(strings.TrimSpace(prefix), "/")
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix)+"/") {
			return strings.TrimSpace(value[len(prefix)+1:])
		}
	}
	return value
}

func importedOpenAICompatConfigIndex(cfg *config.Config, entry config.OpenAICompatibility) int {
	if cfg == nil {
		return -1
	}
	identity := importedOpenAICompatEntryIdentity(entry)
	for index, current := range cfg.OpenAICompatibility {
		if importedOpenAICompatEntryIdentity(current) == identity {
			return index
		}
	}
	return -1
}

func (h *Handler) persistImportedOpenAICompatConfig(ctx context.Context, entries []config.OpenAICompatibility, pending []pendingOpenAICompatImport, result *nineRouterImportResult) {
	if h == nil || h.cfg == nil || len(entries) == 0 || len(pending) == 0 || result == nil {
		return
	}

	h.mu.Lock()
	previous := append([]config.OpenAICompatibility(nil), h.cfg.OpenAICompatibility...)
	h.cfg.OpenAICompatibility = mergeImportedOpenAICompatEntries(h.cfg.OpenAICompatibility, entries)
	h.cfg.SanitizeOpenAICompatibility()
	if strings.TrimSpace(h.configFilePath) == "" {
		h.cfg.OpenAICompatibility = previous
		h.mu.Unlock()
		for _, item := range pending {
			result.Failed = append(result.Failed, map[string]any{
				"index": item.recordIndex, "provider": item.node.ID,
				"error": "configuration file path is empty; OpenAI-compatible key was not persisted",
			})
		}
		return
	}
	if errSave := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); errSave != nil {
		h.cfg.OpenAICompatibility = previous
		h.mu.Unlock()
		for _, item := range pending {
			result.Failed = append(result.Failed, map[string]any{
				"index": item.recordIndex, "provider": item.node.ID,
				"error": fmt.Sprintf("failed to persist OpenAI-compatible key: %v", errSave),
			})
		}
		return
	}
	snapshot := h.reloadSnapshotConfigLocked()
	configSnapshot := h.cfg.CloneForRuntime()
	h.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	h.reloadConfigAfterManagementSave(ctx, snapshot)

	for _, item := range pending {
		entry := config.OpenAICompatibility{
			Name:    item.configName,
			Prefix:  strings.Trim(item.node.Prefix, "/"),
			BaseURL: item.node.BaseURL,
		}
		if configSnapshot != nil {
			if index := importedOpenAICompatConfigIndex(configSnapshot, entry); index >= 0 {
				entry = configSnapshot.OpenAICompatibility[index]
			}
		}
		configIndex := importedOpenAICompatConfigIndex(configSnapshot, entry)
		auths, errAuth := h.ensureOpenAICompatConfigAuths(ctx, entry, configIndex)
		if errAuth != nil {
			result.Failed = append(result.Failed, map[string]any{
				"index": item.recordIndex, "provider": item.node.ID,
				"error": errAuth.Error(),
			})
			continue
		}
		auth := findImportedOpenAICompatAuth(auths, item.apiKey, item.proxyURL)
		if auth == nil {
			result.Failed = append(result.Failed, map[string]any{
				"index": item.recordIndex, "provider": item.node.ID,
				"error": "persisted OpenAI-compatible key could not be bound to a runtime auth",
			})
			continue
		}
		auth.EnsureIndex()
		summary := importedAuthSummary{
			Name:      fmt.Sprintf("config:openai-compatibility[%d]", configIndex),
			Provider:  util.OpenAICompatibleProviderKey(entry.Name),
			AuthIndex: auth.Index,
			Storage:   "config",
		}
		if configIndex >= 0 {
			summary.ConfigIndex = &configIndex
		}
		if email := importStringValue(item.record, "email", "name", "displayName", "display_name"); email != "" {
			summary.Email = email
		}
		result.Imported = append(result.Imported, summary)
	}
}

func (h *Handler) ensureOpenAICompatConfigAuths(ctx context.Context, entry config.OpenAICompatibility, configIndex int) ([]*coreauth.Auth, error) {
	if h == nil || h.authManager == nil {
		return nil, fmt.Errorf("core auth manager unavailable")
	}
	generated, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config:      &config.Config{OpenAICompatibility: []config.OpenAICompatibility{entry}},
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		return nil, fmt.Errorf("failed to synthesize OpenAI-compatible runtime auth: %w", errSynthesize)
	}
	out := make([]*coreauth.Auth, 0, len(generated))
	for _, auth := range generated {
		if auth == nil {
			continue
		}
		if configIndex >= 0 && auth.Attributes != nil {
			auth.Attributes[coreauth.AttributeConfigIndex] = strconv.Itoa(configIndex)
		}
		if existing, ok := h.authManager.GetByID(auth.ID); ok && existing != nil {
			out = append(out, existing)
			continue
		}
		registered, errRegister := h.authManager.Register(coreauth.WithSkipPersist(ctx), auth)
		if errRegister != nil {
			return nil, fmt.Errorf("failed to register OpenAI-compatible runtime auth: %w", errRegister)
		}
		if registered != nil {
			out = append(out, registered)
		}
	}
	return out, nil
}

func findImportedOpenAICompatAuth(auths []*coreauth.Auth, apiKey, proxyURL string) *coreauth.Auth {
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		key := ""
		if auth.Attributes != nil {
			key = strings.TrimSpace(auth.Attributes["api_key"])
		}
		if key == "" && auth.Metadata != nil {
			key = strings.TrimSpace(importStringValue(auth.Metadata, "api_key", "apiKey"))
		}
		if key != strings.TrimSpace(apiKey) {
			continue
		}
		if strings.TrimSpace(auth.ProxyURL) == strings.TrimSpace(proxyURL) {
			return auth
		}
	}
	return nil
}
