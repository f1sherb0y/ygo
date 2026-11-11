package conformance

import (
	"fmt"
	"unicode/utf16"

	"github.com/Deln0r/ygo"
)

func at(n int, length uint64) uint64     { return uint64(n) * (length + 1) >> 16 }
func within(n int, length uint64) uint64 { return uint64(n) * length >> 16 }

// jsValue turns a decoded JSON value into what JavaScript would store:
// whole numbers are integers there.
func jsValue(v any) any {
	switch v := v.(type) {
	case float64:
		if v == float64(int64(v)) {
			return int64(v)
		}
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = jsValue(v[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k := range v {
			out[k] = jsValue(v[k])
		}
		return out
	}
	return v
}

func jsAttrs(attrs map[string]any) ygo.Attrs {
	if attrs == nil {
		return nil
	}
	return jsValue(attrs).(map[string]any)
}

func dumpText(t *ygo.Text) []any {
	out := []any{}
	for _, op := range t.ToDelta() {
		entry := map[string]any{}
		if op.Embed != nil {
			entry["insert"] = op.Embed
		} else {
			entry["insert"] = op.Insert
		}
		if len(op.Attributes) > 0 {
			entry["attributes"] = map[string]any(op.Attributes)
		}
		out = append(out, entry)
	}
	return out
}

func dumpValue(v any) any {
	switch v := v.(type) {
	case *ygo.Text:
		return map[string]any{"$text": dumpText(v)}
	case *ygo.XmlText:
		return map[string]any{"$text": dumpText(&v.Text)}
	case *ygo.Map:
		out := map[string]any{}
		v.Range(func(key string, value any) bool { out[key] = dumpValue(value); return true })
		return map[string]any{"$map": out}
	case *ygo.Array:
		out := []any{}
		v.Range(func(_ uint64, value any) bool { out = append(out, dumpValue(value)); return true })
		return map[string]any{"$array": out}
	case *ygo.XmlElement:
		attrs := map[string]any{}
		for k, value := range v.Attributes() {
			attrs[k] = value
		}
		children := []any{}
		v.Range(func(_ uint64, child any) bool { children = append(children, dumpValue(child)); return true })
		return map[string]any{"$element": v.NodeName(), "attrs": attrs, "children": children}
	}
	return v
}

func dump(d *ygo.Doc) map[string]any {
	arr := []any{}
	ygo.NewArray(d, "arr").Range(func(_ uint64, value any) bool { arr = append(arr, dumpValue(value)); return true })
	xml := []any{}
	ygo.NewXmlFragment(d, "xml").Range(func(_ uint64, child any) bool { xml = append(xml, dumpValue(child)); return true })
	return map[string]any{
		"text": dumpText(ygo.NewText(d, "text")),
		"arr":  arr,
		"map":  dumpValue(ygo.NewMap(d, "map")).(map[string]any)["$map"],
		"xml":  xml,
	}
}

func applyStep(d *ygo.Doc, step Step) error {
	text, arr, m, xml := ygo.NewText(d, "text"), ygo.NewArray(d, "arr"), ygo.NewMap(d, "map"), ygo.NewXmlFragment(d, "xml")
	txn := d.WriteTxn()
	defer txn.Commit()
	clip := func(length uint64) (uint64, uint64) {
		p := within(step.N, length)
		return p, min(uint64(step.K), length-p)
	}
	// With step.Whole, text positions move off the middle of a surrogate
	// pair: a start back, an end forward.
	var units []uint16
	if step.Whole {
		units = utf16.Encode([]rune(text.String()))
	}
	low := func(p uint64) bool { return p > 0 && p < uint64(len(units)) && units[p]&0xfc00 == 0xdc00 }
	back := func(p uint64) uint64 {
		if low(p) {
			return p - 1
		}
		return p
	}
	textRange := func() (uint64, uint64) {
		p, k := clip(text.Length())
		end := p + k
		if low(end) {
			end++
		}
		return back(p), end - back(p)
	}
	switch step.Op {
	case "tins":
		if step.Attrs != nil {
			return text.InsertWithAttributes(txn, back(at(step.N, text.Length())), step.S, jsAttrs(step.Attrs))
		}
		return text.Insert(txn, back(at(step.N, text.Length())), step.S)
	case "tdel":
		if text.Length() > 0 {
			p, k := textRange()
			return text.Delete(txn, p, k)
		}
	case "tfmt":
		if text.Length() > 0 {
			p, k := textRange()
			return text.Format(txn, p, k, jsAttrs(step.Attrs))
		}
	case "temb":
		if step.Attrs != nil {
			return text.InsertEmbedWithAttributes(txn, at(step.N, text.Length()), jsValue(step.Val), jsAttrs(step.Attrs))
		}
		return text.InsertEmbed(txn, at(step.N, text.Length()), jsValue(step.Val))
	case "ains":
		arr.InsertRange(txn, at(step.N, arr.Len()), jsValue(step.Vals).([]any))
	case "adel":
		if arr.Len() > 0 {
			p, k := clip(arr.Len())
			arr.Delete(txn, p, k)
		}
	case "mset":
		m.Set(txn, step.Key, jsValue(step.Val))
	case "mdel":
		m.Delete(txn, step.Key)
	case "mtext":
		return m.SetText(txn, step.Key).Insert(txn, 0, step.S)
	case "amap":
		arr.InsertMap(txn, at(step.N, arr.Len())).Set(txn, step.Key, jsValue(step.Val))
	case "xel":
		el := xml.InsertXmlElement(txn, at(step.N, xml.Length()), step.Tag)
		el.SetAttribute(txn, step.Attr[0], step.Attr[1])
		return el.InsertXmlText(txn, 0).Insert(txn, 0, step.Text)
	case "xdel":
		if xml.Length() > 0 {
			p, k := clip(xml.Length())
			xml.Delete(txn, p, k)
		}
	default:
		return fmt.Errorf("unknown op %q", step.Op)
	}
	return nil
}
