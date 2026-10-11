package webdriver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/adeelahmad/tether/internal/cdp"
)

// printRequest is the W3C Print Page body. Lengths are in centimetres.
type printRequest struct {
	Orientation *string  `json:"orientation"`
	Scale       *float64 `json:"scale"`
	Background  *bool    `json:"background"`
	Page        *struct {
		Width  *float64 `json:"width"`
		Height *float64 `json:"height"`
	} `json:"page"`
	Margin *struct {
		Top    *float64 `json:"top"`
		Bottom *float64 `json:"bottom"`
		Left   *float64 `json:"left"`
		Right  *float64 `json:"right"`
	} `json:"margin"`
	ShrinkToFit *bool             `json:"shrinkToFit"`
	PageRanges  []json.RawMessage `json:"pageRanges"`
}

const (
	cmPerInch   = 2.54
	minPageSize = cmPerInch / 72 // one point
)

var pageRange = regexp.MustCompile(`^\s*(\d*)\s*(-\s*(\d*))?\s*$`)

// pdfOptions validates a Print Page body and converts it for Chrome.
func pdfOptions(body []byte) (cdp.PDFOptions, error) {
	var req printRequest
	if err := decodeBody(body, &req); err != nil {
		return cdp.PDFOptions{}, err
	}
	bad := func(msg string) (cdp.PDFOptions, error) {
		return cdp.PDFOptions{}, wdErr(ErrInvalidArgument, msg)
	}
	o := cdp.PDFOptions{Scale: 1, PrintBackground: false}
	if req.Orientation != nil {
		switch *req.Orientation {
		case "portrait":
		case "landscape":
			o.Landscape = true
		default:
			return bad("orientation must be portrait or landscape")
		}
	}
	if req.Scale != nil {
		if *req.Scale < 0.1 || *req.Scale > 2 {
			return bad("scale must be between 0.1 and 2")
		}
		o.Scale = *req.Scale
	}
	if req.Background != nil {
		o.PrintBackground = *req.Background
	}
	length := func(v *float64, def, lo float64, name string) (float64, error) {
		if v == nil {
			return def / cmPerInch, nil
		}
		if *v < lo {
			return 0, wdErr(ErrInvalidArgument, name+" is too small")
		}
		return *v / cmPerInch, nil
	}
	var err error
	var pw, ph *float64
	if req.Page != nil {
		pw, ph = req.Page.Width, req.Page.Height
	}
	if o.PaperWidth, err = length(pw, 21.59, minPageSize, "page width"); err != nil {
		return o, err
	}
	if o.PaperHeight, err = length(ph, 27.94, minPageSize, "page height"); err != nil {
		return o, err
	}
	var mt, mb, ml, mr *float64
	if req.Margin != nil {
		mt, mb, ml, mr = req.Margin.Top, req.Margin.Bottom, req.Margin.Left, req.Margin.Right
	}
	for _, m := range []struct {
		v    *float64
		dst  *float64
		name string
	}{{mt, &o.MarginTop, "top margin"}, {mb, &o.MarginBottom, "bottom margin"}, {ml, &o.MarginLeft, "left margin"}, {mr, &o.MarginRight, "right margin"}} {
		if *m.dst, err = length(m.v, 1, 0, m.name); err != nil {
			return o, err
		}
	}
	var ranges []string
	for _, raw := range req.PageRanges {
		var n float64
		var s string
		switch {
		case json.Unmarshal(raw, &n) == nil:
			if n < 1 || n != float64(int(n)) {
				return bad("page numbers must be positive integers")
			}
			ranges = append(ranges, strconv.Itoa(int(n)))
		case json.Unmarshal(raw, &s) == nil:
			m := pageRange.FindStringSubmatch(s)
			if m == nil || (m[1] == "" && m[3] == "") {
				return bad("invalid page range " + strconv.Quote(s))
			}
			r := m[1]
			if m[2] != "" {
				r += "-" + m[3]
			}
			ranges = append(ranges, r)
		default:
			return bad("pageRanges must hold numbers or range strings")
		}
	}
	o.PageRanges = strings.Join(ranges, ",")
	return o, nil
}

// print implements Print Page for the current window's top-level page.
func (h *Handler) print(ctx context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	opts, err := pdfOptions(body)
	if err != nil {
		return nil, err
	}
	pdf, err := ws.s.Tab.PrintPDF(ctx, opts)
	if err != nil {
		var pe *cdp.ProtocolError
		if errors.As(err, &pe) && strings.Contains(strings.ToLower(pe.Message), "not implemented") {
			return nil, wdErr(ErrUnsupportedOperation, "this Chrome cannot print to PDF (older Chrome only prints when headless)")
		}
		if errors.As(err, &pe) && strings.Contains(strings.ToLower(pe.Message), "page range") {
			return nil, wdErr(ErrInvalidArgument, pe.Message)
		}
		return nil, err
	}
	return base64.StdEncoding.EncodeToString(pdf), nil
}
