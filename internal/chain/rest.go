package chain

// rest.go — helpers para leer el JSON crudo de VBR (map[string]any) sin declarar
// un struct por cada modelo, y para paginar colecciones {data, pagination}.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"yogachain/internal/dbg"
	"yogachain/internal/vbr"
)

const pageSize = 100 // "cargar de a 100"

type obj = map[string]any

func str(m obj, k string) string {
	if m == nil {
		return ""
	}
	switch v := m[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func num(m obj, k string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func boolean(m obj, k string) bool {
	if m == nil {
		return false
	}
	b, _ := m[k].(bool)
	return b
}

func sub(m obj, k string) obj {
	if m == nil {
		return nil
	}
	o, _ := m[k].(map[string]any)
	return o
}

func arr(m obj, k string) []any {
	if m == nil {
		return nil
	}
	a, _ := m[k].([]any)
	return a
}

func strs(m obj, k string) []string {
	var out []string
	for _, v := range arr(m, k) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

const gb = 1024 * 1024 * 1024

func toGB(m obj, k string) float64 { return num(m, k) / gb }

// getObj hace un GET y decodifica un objeto.
func getObj(ctx context.Context, s *vbr.Session, path string) (obj, error) {
	raw, err := vbr.Get(ctx, s, path)
	if err != nil {
		return nil, err
	}
	var o obj
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, &vbr.APIError{Status: 502, Message: "Unexpected JSON from " + path}
	}
	return o, nil
}

// getAll pagina una coleccion de a pageSize hasta agotarla. `path` puede traer
// query string; skip/limit se agregan al final. max=0 -> sin tope.
func getAll(ctx context.Context, s *vbr.Session, path string, max int) ([]obj, error) {
	var out []obj
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for skip := 0; ; skip += pageSize {
		page, err := getObj(ctx, s, fmt.Sprintf("%s%sskip=%d&limit=%d", path, sep, skip, pageSize))
		if err != nil {
			return out, err
		}
		data := arr(page, "data")
		for _, it := range data {
			if o, ok := it.(map[string]any); ok {
				out = append(out, o)
			}
		}
		dbg.Logf("paged %s: +%d (total %d)", path, len(data), len(out))
		if skip == 0 && len(data) > 0 {
			// Alpha: el primer item crudo de cada coleccion queda en el log para
			// validar el schema real contra lo asumido (ver // LAB).
			if b, err := json.Marshal(data[0]); err == nil {
				dbg.Logf("sample %s: %s", path, dbg.Clip(string(b), 700))
			}
		}
		if len(data) < pageSize || (max > 0 && len(out) >= max) {
			return out, nil
		}
	}
}
