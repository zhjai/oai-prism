package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/oai-prism/oaiprism/internal/account"
)

func (server *Server) renameAPIKey(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Name *string `json:"name"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Name == nil {
		writeAdminErr(writer, http.StatusBadRequest, "name 必须为字符串")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeAdminErr(writer, http.StatusBadRequest, "请求体必须为单个 JSON 对象")
		return
	}
	if strings.IndexFunc(*body.Name, unicode.IsControl) >= 0 {
		writeAdminErr(writer, http.StatusBadRequest, "名称不能包含控制字符")
		return
	}
	name := strings.TrimSpace(*body.Name)
	if utf8.RuneCountInString(name) > 64 {
		writeAdminErr(writer, http.StatusBadRequest, "名称最多 64 个字符")
		return
	}
	if server.sqlite == nil {
		writeAdminErr(writer, http.StatusServiceUnavailable, "SQLite 存储未初始化")
		return
	}
	if err := server.sqlite.RenameAPIKey(request.PathValue("key"), name); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, account.ErrAPIKeyNotFound) {
			status = http.StatusNotFound
		}
		writeAdminErr(writer, status, err.Error())
		return
	}
	server.keys.Invalidate()
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ok", "name": name})
}
