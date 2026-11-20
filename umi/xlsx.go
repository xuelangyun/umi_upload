package umi

import (
	"archive/zip"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	reBadXMLCtrl = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f]`)
	reBadSheetCh = regexp.MustCompile(`[\[\]\*\?/\\:]`)
)

type XLSXSheet struct {
	Name   string
	Rows   [][]any
	Widths []int
	Filter bool
	Freeze bool
}

func xlsxColName(n int) string {
	s := ""
	for {
		r := n % 26
		n = n / 26
		s = string(rune('A'+r)) + s
		if n == 0 {
			return s
		}
		n--
	}
}

func xmlEscape(s string) string {
	s = reBadXMLCtrl.ReplaceAllString(s, "")
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch r {
		case '&':
			sb.WriteString("&amp;")
		case '<':
			sb.WriteString("&lt;")
		case '>':
			sb.WriteString("&gt;")
		case '"':
			sb.WriteString("&quot;")
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func buildSheetXML(sh XLSXSheet) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	sb.WriteString(`<sheetViews><sheetView workbookViewId="0">`)
	if sh.Freeze {
		sb.WriteString(`<pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/>`)
	}
	sb.WriteString(`</sheetView></sheetViews><sheetFormatPr defaultRowHeight="15"/>`)
	if len(sh.Widths) > 0 {
		sb.WriteString(`<cols>`)
		for i, w := range sh.Widths {
			fmt.Fprintf(&sb, `<col min="%d" max="%d" width="%d" customWidth="1"/>`, i+1, i+1, w)
		}
		sb.WriteString(`</cols>`)
	}
	sb.WriteString(`<sheetData>`)
	for rIdx, row := range sh.Rows {
		rNum := rIdx + 1
		fmt.Fprintf(&sb, `<row r="%d">`, rNum)
		for cIdx, v := range row {
			ref := fmt.Sprintf("%s%d", xlsxColName(cIdx), rNum)
			st := ""
			if rNum == 1 {
				st = ` s="1"`
			}
			if v == nil || v == "" {
				fmt.Fprintf(&sb, `<c r="%s"%s/>`, ref, st)
				continue
			}
			switch num := v.(type) {
			case int:
				fmt.Fprintf(&sb, `<c r="%s"%s><v>%d</v></c>`, ref, st, num)
			case int64:
				fmt.Fprintf(&sb, `<c r="%s"%s><v>%d</v></c>`, ref, st, num)
			case float64:
				numSt := ` s="2"`
				fmt.Fprintf(&sb, `<c r="%s"%s><v>%s</v></c>`, ref, numSt, strconv.FormatFloat(num, 'f', -1, 64))
			default:
				fmt.Fprintf(&sb, `<c r="%s"%s t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, st, xmlEscape(fmt.Sprint(v)))
			}
		}
		sb.WriteString(`</row>`)
	}
	sb.WriteString(`</sheetData>`)
	if sh.Filter && len(sh.Rows) > 0 && len(sh.Rows[0]) > 0 {
		fmt.Fprintf(&sb, `<autoFilter ref="A1:%s%d"/>`, xlsxColName(len(sh.Rows[0])-1), max(len(sh.Rows), 1))
	}
	sb.WriteString(`</worksheet>`)
	return sb.String()
}

// BuildXLSX generates a multi-sheet OpenXML .xlsx file using only Go standard library (archive/zip).
func BuildXLSX(sheets []XLSXSheet) ([]byte, error) {
	names := make([]string, len(sheets))
	for i, sh := range sheets {
		clean := reBadSheetCh.ReplaceAllString(sh.Name, "_")
		runes := []rune(clean)
		if len(runes) > 31 {
			clean = string(runes[:31])
		}
		names[i] = xmlEscape(clean)
	}

	var ct strings.Builder
	ct.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	ct.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/>`)
	ct.WriteString(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	ct.WriteString(`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	for i := range sheets {
		fmt.Fprintf(&ct, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i+1)
	}
	ct.WriteString(`</Types>`)

	rels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`

	var wb strings.Builder
	wb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	for i, n := range names {
		fmt.Fprintf(&wb, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, n, i+1, i+1)
	}
	wb.WriteString(`</sheets></workbook>`)

	var wbrels strings.Builder
	wbrels.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := range sheets {
		fmt.Fprintf(&wbrels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i+1, i+1)
	}
	fmt.Fprintf(&wbrels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`, len(sheets)+1)

	styles := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<numFmts count="1"><numFmt numFmtId="164" formatCode="0.00"/></numFmts>` +
		`<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts>` +
		`<fills count="3"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FFDFF1EA"/></patternFill></fill></fills>` +
		`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>` +
		`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
		`<cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="2" borderId="0" xfId="0" applyFont="1" applyFill="1"/>` +
		`<xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/></cellXfs>` +
		`<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	writeFile := func(name, content string) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(content))
		return err
	}

	if err := writeFile("[Content_Types].xml", ct.String()); err != nil {
		return nil, err
	}
	if err := writeFile("_rels/.rels", rels); err != nil {
		return nil, err
	}
	if err := writeFile("xl/workbook.xml", wb.String()); err != nil {
		return nil, err
	}
	if err := writeFile("xl/_rels/workbook.xml.rels", wbrels.String()); err != nil {
		return nil, err
	}
	if err := writeFile("xl/styles.xml", styles); err != nil {
		return nil, err
	}
	for i, sh := range sheets {
		if err := writeFile(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), buildSheetXML(sh)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
