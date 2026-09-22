package doc

import (
	"fmt"
	"strings"
)

// Minimal PresentationML package, built with the standard library only. The
// package carries one slide master + one layout and N slides; each slide holds
// a title shape and a body shape whose paragraphs are the skeleton bullets.

const pptContentTypesHead = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>
<Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml"/>
<Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"/>`

const pptxRelsRoot = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/>
</Relationships>`

const pptxMaster = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sldMaster xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/></p:spTree></p:cSld>
<p:sldLayoutIdLst><p:sldLayoutId id="2147483649" r:id="rId1"/></p:sldLayoutIdLst>
</p:sldMaster>`

const pptxMasterRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>
</Relationships>`

const pptxLayout = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sldLayout xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" type="blank">
<p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/></p:spTree></p:cSld>
</p:sldLayout>`

func pptxPresentation(n int) string {
	var ids strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&ids, `<p:sldId id="%d" r:id="rId%d"/>`, 256+i, 2+i)
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<p:sldMasterIdLst><p:sldMasterId id="2147483648" r:id="rId1"/></p:sldMasterIdLst>
<p:sldIdLst>` + ids.String() + `</p:sldIdLst>
<p:sldSz cx="9144000" cy="6858000"/>
</p:presentation>`
}

func pptxPresentationRels(n int) string {
	var rels strings.Builder
	rels.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + "\n")
	rels.WriteString(`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="slideMasters/slideMaster1.xml"/>` + "\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide%d.xml"/>`+"\n", 2+i, i+1)
	}
	rels.WriteString(`</Relationships>`)
	return rels.String()
}

func pptxSlide(title string, bullets []string) string {
	var body strings.Builder
	for _, b := range bullets {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		body.WriteString(`<a:p><a:r><a:rPr lang="zh-CN"/><a:t>`)
		body.WriteString(xmlEscape(b))
		body.WriteString(`</a:t></a:r></a:p>`)
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<p:cSld><p:spTree>
<p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>
<p:grpSpPr/>
<p:sp><p:nvSpPr><p:cNvPr id="2" name="Title"/><p:cNvSpPr/><p:nvPr/></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:pPr algn="ctr"/><a:r><a:rPr lang="zh-CN" sz="3600"/><a:t>` +
		xmlEscape(title) +
		`</a:t></a:r></a:p></p:txBody></p:sp>
<p:sp><p:nvSpPr><p:cNvPr id="3" name="Body"/><p:cNvSpPr/><p:nvPr/></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/>` +
		body.String() +
		`</p:txBody></p:sp>
</p:spTree></p:cSld>
</p:sld>`
}

// renderPPTX builds a minimal .pptx: one slide master/layout plus a slide per
// skeleton entry (title + bullets).
func renderPPTX(sk Skeleton) ([]byte, error) {
	if len(sk.Slides) == 0 {
		return nil, fmt.Errorf("doc: pptx skeleton has no slides")
	}
	entries := map[string]string{
		"[Content_Types].xml":               pptxContentTypes(len(sk.Slides)),
		"_rels/.rels":                       pptxRelsRoot,
		"ppt/presentation.xml":              pptxPresentation(len(sk.Slides)),
		"ppt/_rels/presentation.xml.rels":   pptxPresentationRels(len(sk.Slides)),
		"ppt/slideMasters/slideMaster1.xml": pptxMaster,
		"ppt/slideMasters/_rels/slideMaster1.xml.rels": pptxMasterRels,
		"ppt/slideLayouts/slideLayout1.xml":            pptxLayout,
	}
	for i, s := range sk.Slides {
		entries[fmt.Sprintf("ppt/slides/slide%d.xml", i+1)] = pptxSlide(s.Title, s.Bullets)
	}
	return buildZipDoc(entries), nil
}

func pptxContentTypes(n int) string {
	var sb strings.Builder
	sb.WriteString(pptContentTypesHead)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, `<Override PartName="/ppt/slides/slide%d.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>`, i+1)
	}
	sb.WriteString(`</Types>`)
	return sb.String()
}
