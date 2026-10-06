package facade

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// ModelInfo 是 /v1/models 的条目。
//
// Name 是展示名（label），与 ID 分离：ID 是调用时要传的名字，
// Name 给 UI 列表用。没配 label 的模型不带 name 字段。
type ModelInfo struct {
	ID                     string   `json:"id"`
	Object                 string   `json:"object"`
	Created                int64    `json:"created"`
	OwnedBy                string   `json:"owned_by"`
	Name                   string   `json:"name,omitempty"`
	UpstreamModel          string   `json:"upstream_model,omitempty"`
	ReasoningEffort        string   `json:"reasoning_effort,omitempty"`
	ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`
	Default                bool     `json:"default,omitempty"`
}

// ModelList 是 /v1/models 的返回。
type ModelList struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// handleModels 列出对外可用的模型名。
//
// 默认按当前 Key 的账号范围取得上游目录，并过滤本地别名。
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Facade.ModelCatalog.Enabled {
		catalogs, err := h.liveCatalog(r)
		if err != nil && !errors.Is(err, errCatalogIncomplete) {
			writeCatalogError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ModelList{Object: "list", Data: h.liveModelInfos(catalogs, !errors.Is(err, errCatalogIncomplete))})
		return
	}
	f := &h.cfg.Facade

	names := make([]string, 0, len(f.Models)+1)
	for name := range f.Models {
		names = append(names, name)
	}
	// 保证默认模型一定在列表里（可能没显式写进映射表）。
	found := false
	for _, n := range names {
		if n == f.DefaultModel {
			found = true
			break
		}
	}
	if !found && f.DefaultModel != "" {
		names = append(names, f.DefaultModel)
	}
	sort.Strings(names)

	created := time.Now().Unix()
	data := make([]ModelInfo, 0, len(names))
	for _, n := range names {
		info := ModelInfo{
			ID:      n,
			Object:  "model",
			Created: created,
			OwnedBy: "oaiprism",
		}
		if m, ok := f.Models[n]; ok {
			info.Name = m.Label
		}
		data = append(data, info)
	}
	writeJSON(w, http.StatusOK, ModelList{Object: "list", Data: data})
}

// handleModelByID 返回单个模型信息。
func (h *Handler) handleModelByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "缺少模型 ID")
		return
	}
	if h.cfg.Facade.ModelCatalog.Enabled {
		catalogs, err := h.liveCatalog(r)
		if err != nil && !errors.Is(err, errCatalogIncomplete) {
			writeCatalogError(w, err)
			return
		}
		for _, info := range h.liveModelInfos(catalogs, !errors.Is(err, errCatalogIncomplete)) {
			if info.ID == id {
				writeJSON(w, http.StatusOK, info)
				return
			}
		}
		if errors.Is(err, errCatalogIncomplete) {
			writeCatalogError(w, err)
			return
		}
		writeErrorCode(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "模型当前不可用，可用模型见 GET /v1/models")
		return
	}
	f := &h.cfg.Facade
	if _, ok := f.Models[id]; !ok && id != f.DefaultModel {
		writeError(w, http.StatusNotFound, "invalid_request_error",
			"未知模型 "+id+"（可用模型见 GET /v1/models）")
		return
	}
	info := ModelInfo{
		ID:      id,
		Object:  "model",
		Created: time.Now().Unix(),
		OwnedBy: "oaiprism",
	}
	if m, ok := f.Models[id]; ok {
		info.Name = m.Label
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *Handler) liveModelInfos(catalogs []accountCatalog, complete bool) []ModelInfo {
	data := make([]ModelInfo, 0)
	seen := map[string]bool{}
	upstream := map[string][]prism.UpstreamModel{}
	created := time.Now().Unix()
	add := func(info ModelInfo) {
		if seen[info.ID] {
			// Shared model metadata describes the same account union as the list.
			// Missing upstream effort metadata means the supported set is unknown.
			if info.ID == info.UpstreamModel {
				for i := range data {
					if data[i].ID != info.ID {
						continue
					}
					if len(info.ReasoningEfforts) == 0 || len(data[i].ReasoningEfforts) == 0 {
						data[i].ReasoningEfforts = nil
						break
					}
					merged := append([]string(nil), data[i].ReasoningEfforts...)
					for _, effort := range info.ReasoningEfforts {
						found := false
						for _, existing := range merged {
							found = found || existing == effort
						}
						if !found {
							merged = append(merged, effort)
						}
					}
					data[i].ReasoningEfforts = merged
					break
				}
			}
			return
		}
		seen[info.ID] = true
		info.Object, info.Created, info.OwnedBy = "model", created, "oaiprism"
		data = append(data, info)
	}
	for _, c := range catalogs {
		for _, m := range c.models {
			upstream[m.ID] = append(upstream[m.ID], m)
			effort := h.catalogDefaultEffort(m)
			add(ModelInfo{ID: m.ID, Name: m.Label, UpstreamModel: m.ID, ReasoningEfforts: m.Efforts, ReasoningEffort: effort, DefaultReasoningEffort: effort})
			for _, e := range m.Efforts {
				if e != m.DefaultEffort {
					add(ModelInfo{ID: m.ID + "-" + e, Name: m.Label + " (" + e + ")", UpstreamModel: m.ID, ReasoningEffort: e})
				}
			}
		}
	}
	names := make([]string, 0, len(h.cfg.Facade.Models))
	for name := range h.cfg.Facade.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m := h.cfg.Facade.Models[name]
		target := m.Model
		if target == "" {
			target = name
		}
		allowed := false
		for _, mUp := range upstream[target] {
			if modelSupportsEffort(mUp, m.ReasoningEffort) {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		add(ModelInfo{ID: name, Name: m.Label, UpstreamModel: target, ReasoningEffort: m.ReasoningEffort})
	}
	def := h.cfg.Facade.DefaultModel
	if complete && !seen[def] && len(data) > 0 {
		def = data[0].ID
	}
	for i := range data {
		data[i].Default = data[i].ID == def
	}
	return data
}
