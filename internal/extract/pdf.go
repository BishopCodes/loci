package extract

import (
	"bytes"
	"fmt"

	"github.com/ledongthuc/pdf"
)

// gopdf is the pure-Go PDF fallback used when pdftotext isn't on PATH.
// ledongthuc/pdf panics on some malformed files, so it runs guarded.
func gopdf(body []byte) (text string, warning string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pure-go pdf parser panicked: %v", r)
		}
	}()
	reader := bytes.NewReader(body)
	r, err := pdf.NewReader(reader, int64(len(body)))
	if err != nil {
		return "", "", fmt.Errorf("open pdf: %w", err)
	}
	var sb bytes.Buffer
	for i := 0; i < r.NumPage(); i++ {
		p := r.Page(i + 1)
		if p.V.IsNull() {
			continue
		}
		txt, werr := p.GetPlainText(nil)
		if werr != nil {
			warning = fmt.Sprintf("page %d degraded: %v", i+1, werr)
			continue
		}
		sb.WriteString(txt)
		sb.WriteString("\n\n")
	}
	return sb.String(), warning, nil
}
