# -*- coding: utf-8 -*-
"""最小 xlsx 写入器（只用标准库，不需要 openpyxl）：多工作表、表头样式、冻结首行、筛选、列宽、数字/文本单元格。
build_xlsx([(表名, [[单元格…], …], [列宽…], 可选 {'filter': False}), …]) → bytes。每个表第一行当作表头。单元格：str / int / float / None（float 显示两位小数）。"""
import io
import re
import zipfile
from xml.sax.saxutils import escape

_BAD = re.compile('[\x00-\x08\x0b\x0c\x0e-\x1f]')                      # XML 不允许的控制字符


def _col(n):
    s = ''
    while True:
        n, r = divmod(n, 26); s = chr(65 + r) + s
        if n == 0: return s
        n -= 1


def _sheet(rows, widths, flt=True, freeze=True):
    out = ['<?xml version="1.0" encoding="UTF-8" standalone="yes"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">']
    out.append('<sheetViews><sheetView workbookViewId="0">' + ('<pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/>' if freeze else '') + '</sheetView></sheetViews><sheetFormatPr defaultRowHeight="15"/>')
    if widths: out.append('<cols>' + ''.join('<col min="%d" max="%d" width="%s" customWidth="1"/>' % (i + 1, i + 1, w) for i, w in enumerate(widths)) + '</cols>')
    out.append('<sheetData>')
    for r, row in enumerate(rows, 1):
        out.append('<row r="%d">' % r)
        for c, v in enumerate(row):
            ref, st = '%s%d' % (_col(c), r), ' s="1"' if r == 1 else ''
            if v is None or v == '': out.append('<c r="%s"%s/>' % (ref, st))
            elif isinstance(v, (int, float)) and not isinstance(v, bool): out.append('<c r="%s"%s><v>%s</v></c>' % (ref, ' s="2"' if isinstance(v, float) else st, repr(v)))
            else: out.append('<c r="%s"%s t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>' % (ref, st, escape(_BAD.sub('', str(v)))))
        out.append('</row>')
    out.append('</sheetData>')
    if flt and rows and rows[0]: out.append('<autoFilter ref="A1:%s%d"/>' % (_col(len(rows[0]) - 1), max(len(rows), 1)))
    out.append('</worksheet>')
    return ''.join(out)


def build_xlsx(sheets):
    names = [escape(re.sub(r'[\[\]\*\?/\\:]', '_', s[0])[:31]) for s in sheets]       # 先截断再转义，避免把 &amp; 截成半截
    ct = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">'
          '<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/>'
          '<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>'
          '<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>'
          + ''.join('<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>' % (i + 1) for i in range(len(sheets)))
          + '</Types>')
    rels = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
            '<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>')
    wb = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>'
          + ''.join('<sheet name="%s" sheetId="%d" r:id="rId%d"/>' % (n, i + 1, i + 1) for i, n in enumerate(names)) + '</sheets></workbook>')
    wbrels = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
              + ''.join('<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>' % (i + 1, i + 1) for i in range(len(sheets)))
              + '<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>' % (len(sheets) + 1))
    styles = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?><styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">'
              '<numFmts count="1"><numFmt numFmtId="164" formatCode="0.00"/></numFmts>'
              '<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts>'
              '<fills count="3"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FFDFF1EA"/></patternFill></fill></fills>'
              '<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>'
              '<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>'
              '<cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="2" borderId="0" xfId="0" applyFont="1" applyFill="1"/>'
              '<xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/></cellXfs>'
              '<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>')
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, 'w', zipfile.ZIP_DEFLATED) as z:
        z.writestr('[Content_Types].xml', ct); z.writestr('_rels/.rels', rels); z.writestr('xl/workbook.xml', wb)
        z.writestr('xl/_rels/workbook.xml.rels', wbrels); z.writestr('xl/styles.xml', styles)
        for i, sh in enumerate(sheets):
            o = sh[3] if len(sh) > 3 else {}
            z.writestr('xl/worksheets/sheet%d.xml' % (i + 1), _sheet(sh[1], sh[2], o.get('filter', True), o.get('freeze', True)))
    return buf.getvalue()
