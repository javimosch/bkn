package files

import "net/http"

// httpDetect wraps the stdlib sniffer so blobs.go does not import net/http
// just for one call.
func httpDetect(sample []byte) string { return http.DetectContentType(sample) }
