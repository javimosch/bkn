package script

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/javimosch/bkn/internal/files"
)

// maxScriptFileBytes caps what a script may pull into the VM in one read.
// A blob store holds things far larger than a script should ever materialize
// as a JavaScript string.
const maxScriptFileBytes = 8 << 20

// newFilesAPI builds the `bkn.files` namespace.
func (r *Runner) newFilesAPI(throw func(error)) map[string]any {
	if r.files == nil {
		return nil
	}
	fs := r.files

	decode := func(content, encoding string) io.Reader {
		if encoding == "base64" {
			raw, err := base64.StdEncoding.DecodeString(content)
			if err != nil {
				throw(fmt.Errorf("content is not valid base64: %w", err))
			}
			return strings.NewReader(string(raw))
		}
		return strings.NewReader(content)
	}

	return map[string]any{
		"put": func(ns, name, content string, opts map[string]any) any {
			encoding, contentType := "utf8", ""
			var metadata map[string]any
			overwrite := false
			if opts != nil {
				if v, ok := opts["encoding"].(string); ok {
					encoding = v
				}
				if v, ok := opts["contentType"].(string); ok {
					contentType = v
				}
				if v, ok := opts["metadata"].(map[string]any); ok {
					metadata = v
				}
				if v, ok := opts["overwrite"].(bool); ok {
					overwrite = v
				}
			}
			f, err := fs.Put(ns, name, decode(content, encoding), files.PutOptions{
				ContentType: contentType, Metadata: metadata, Overwrite: overwrite,
			})
			if err != nil {
				throw(err)
			}
			return f
		},
		"get": func(ns, name string, opts map[string]any) any {
			f, rc, err := fs.Get(ns, name)
			if errors.Is(err, files.ErrNotFound) || errors.Is(err, files.ErrNoNamespace) {
				return nil
			}
			if err != nil {
				throw(err)
			}
			defer rc.Close()

			if f.Size > maxScriptFileBytes {
				throw(fmt.Errorf("%s/%s is %d bytes, over the %d byte limit a script may read",
					ns, name, f.Size, maxScriptFileBytes))
			}
			raw, err := io.ReadAll(io.LimitReader(rc, maxScriptFileBytes))
			if err != nil {
				throw(err)
			}
			encoding := "utf8"
			if opts != nil {
				if v, ok := opts["encoding"].(string); ok {
					encoding = v
				}
			}
			content := string(raw)
			if encoding == "base64" {
				content = base64.StdEncoding.EncodeToString(raw)
			}
			return map[string]any{
				"name": f.Name, "namespace": f.Namespace, "size": f.Size,
				"content_type": f.ContentType, "sha256": f.SHA256,
				"metadata": f.Metadata, "encoding": encoding, "content": content,
			}
		},
		"show": func(ns, name string) any {
			f, err := fs.Show(ns, name)
			if errors.Is(err, files.ErrNotFound) || errors.Is(err, files.ErrNoNamespace) {
				return nil
			}
			if err != nil {
				throw(err)
			}
			return f
		},
		"list": func(ns string, opts map[string]any) []any {
			limit, offset := 50, 0
			if opts != nil {
				if n, ok := toInt(opts["limit"]); ok {
					limit = n
				}
				if n, ok := toInt(opts["offset"]); ok {
					offset = n
				}
			}
			list, err := fs.List(ns, limit, offset)
			if err != nil {
				throw(err)
			}
			out := make([]any, len(list))
			for i, f := range list {
				out[i] = f
			}
			return out
		},
		"delete": func(ns, name string) bool {
			err := fs.Delete(ns, name)
			if errors.Is(err, files.ErrNotFound) {
				return false
			}
			if err != nil {
				throw(err)
			}
			return true
		},
		"namespaces": func() []any {
			list, err := fs.Namespaces()
			if err != nil {
				throw(err)
			}
			out := make([]any, len(list))
			for i, ns := range list {
				out[i] = ns
			}
			return out
		},
		"sign": func(ns, name string, opts map[string]any) string {
			ttl := 24 * time.Hour
			if opts != nil {
				if v, ok := opts["ttl"].(string); ok {
					d, err := time.ParseDuration(v)
					if err == nil {
						ttl = d
					}
				}
			}
			namespace, err := fs.Namespace(ns)
			if err != nil {
				throw(err)
			}
			if namespace.SigningKey == "" {
				throw(fmt.Errorf("namespace %s has no signing key", ns))
			}
			url, err := files.SignURL(ns, name, namespace.SigningKey, ttl)
			if err != nil {
				throw(err)
			}
			return url
		},
	}
}
