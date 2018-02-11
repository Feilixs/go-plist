package plist

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"runtime"
	"time"
)

type xmlPlistParser struct {
	reader     io.Reader
	xmlDecoder *xml.Decoder
	//whitespaceReplacer *strings.Replacer
}

func (p *xmlPlistParser) error(e string, args ...interface{}) {
	off := p.xmlDecoder.InputOffset()
	panic(fmt.Errorf("%s at offset %d", fmt.Sprintf(e, args...), off))
}

func (p *xmlPlistParser) mismatchedTags(start xml.StartElement, end xml.EndElement) {
	p.error("mismatched opening/closing tags <%s> and </%s>", start.Name.Local, end.Name.Local)
}

func (p *xmlPlistParser) unexpected(token xml.Token) {
	p.error("unexpected XML element `%v`", token)
}

func (p *xmlPlistParser) parseDocument() (pval cfValue, parseError error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(runtime.Error); ok {
				panic(r)
			}
			if _, ok := r.(invalidPlistError); ok {
				parseError = r.(error)
			} else {
				// Wrap all non-invalid-plist errors.
				parseError = plistParseError{"XML", r.(error)}
			}
		}
	}()
	for {
		if token, err := p.xmlDecoder.Token(); err == nil {
			if element, ok := token.(xml.StartElement); ok {
				pval = p.parseXMLElement(element)
				if pval == nil {
					panic(invalidPlistError{"XML", errors.New("no elements encountered")})
				}
				return
			}
		} else {
			// The first XML parse turned out to be invalid:
			// we do not have an XML property list.
			panic(invalidPlistError{"XML", err})
		}
	}
}

func (p *xmlPlistParser) next() xml.Token {
	token, err := p.xmlDecoder.Token()
	if err != nil {
		p.error("%v", err)
	}
	return token
}

func (p *xmlPlistParser) skip() {
	err := p.xmlDecoder.Skip()
	if err != nil {
		p.error("%v", err)
	}
}

// opening tag has been consumed
func (p *xmlPlistParser) getNextString(element xml.StartElement) string {
	token := p.next()

	switch token := token.(type) {
	case xml.CharData:
		s := string(token)
		p.skip() // skip closing tag
		return s
	case xml.EndElement:
		if token.Name.Local != element.Name.Local {
			p.mismatchedTags(element, token)
		}
		return "" // empty string!
	case xml.Comment:
		// nothing
	default:
		p.unexpected(token)
	}

	p.unexpected(token)
	return ""
}

func (p *xmlPlistParser) parseStringElement(element xml.StartElement) cfString {
	return cfString(p.getNextString(element))
}

func (p *xmlPlistParser) parseIntegerElement(element xml.StartElement) *cfNumber {
	s := p.getNextString(element)
	if len(s) == 0 {
		p.error("empty <integer/>")
	}

	if s[0] == '-' {
		s, base := unsignedGetBase(s[1:])
		n := mustParseInt("-"+s, base, 64)
		return &cfNumber{signed: true, value: uint64(n)}
	}

	s, base := unsignedGetBase(s)
	n := mustParseUint(s, base, 64)
	return &cfNumber{signed: false, value: n}
}

func (p *xmlPlistParser) parseRealElement(element xml.StartElement) *cfReal {
	s := p.getNextString(element)

	n := mustParseFloat(s, 64)
	return &cfReal{wide: true, value: n}

}

func (p *xmlPlistParser) parseDateElement(element xml.StartElement) cfDate {
	s := p.getNextString(element)

	t, err := time.ParseInLocation(time.RFC3339, s, time.UTC)
	if err != nil {
		p.error("%v", err)
	}

	return cfDate(t)
}

func (p *xmlPlistParser) parseDataElement(element xml.StartElement) cfData {
	s := []byte(p.getNextString(element))

	offset := 0
	for i, v := range s {
		if v != ' ' && v != '\t' && v != '\n' && v != '\r' {
			if offset != i {
				s[offset] = s[i]
			}
			offset++
		}
	}
	s = s[:offset]

	l := base64.StdEncoding.DecodedLen(offset)
	bytes := make([]uint8, l)

	var err error
	l, err = base64.StdEncoding.Decode(bytes, s)
	if err != nil {
		p.error("%v", err)
	}

	return cfData(bytes[:l])
}

func (p *xmlPlistParser) realizeKeysAndValues(keys []string, values []cfValue) cfValue {
	if len(keys) != len(values) {
		p.error("missing value in dictionary")
	}

	if len(keys) == 1 && keys[0] == "CF$UID" {
		if integer, ok := values[0].(*cfNumber); ok {
			return cfUID(integer.value)
		}
	}

	return &cfDictionary{keys: keys, values: values}
}

func (p *xmlPlistParser) parseDictionary(element xml.StartElement) cfValue {
	keys := make([]string, 0, 32)
	values := make([]cfValue, 0, 32)
outer:
	for {
		token := p.next()

		switch token := token.(type) {
		case xml.EndElement:
			if token.Name.Local == "dict" {
				return p.realizeKeysAndValues(keys, values)
			} else {
				p.mismatchedTags(element, token)
			}
		case xml.StartElement:
			if token.Name.Local == "key" {
				k := p.getNextString(token)
				keys = append(keys, k)
			} else {
				if len(keys) != len(values)+1 {
					p.error("missing key in dictionary")
				}
				values = append(values, p.parseXMLElement(token))
			}
		case xml.CharData, xml.Comment:
			continue outer // ignore all extraelemental data
		default:
			p.unexpected(token)
		}
	}
	return nil // shouldn't get here
}

func (p *xmlPlistParser) parseArray(element xml.StartElement) *cfArray {
	values := make([]cfValue, 0, 32)
outer:
	for {
		token := p.next()

		switch token := token.(type) {
		case xml.EndElement:
			if token.Name.Local == "array" {
				break outer
			}
			p.mismatchedTags(element, token)
		case xml.StartElement:
			values = append(values, p.parseXMLElement(token))
		case xml.CharData, xml.Comment:
			continue outer // ignore all extraelemental data
		default:
			p.unexpected(token)
		}
	}
	return &cfArray{values}
}

func (p *xmlPlistParser) parseXMLElement(element xml.StartElement) cfValue {
	switch element.Name.Local {
	case "plist":
		// a <plist> should contain only one sub-element; we can safely recurse in here
		for {
			token := p.next()

			if el, ok := token.(xml.EndElement); ok && el.Name.Local == "plist" {
				break
			}

			if el, ok := token.(xml.StartElement); ok {
				return p.parseXMLElement(el)
			}
		}
		return nil
	case "string":
		return p.parseStringElement(element)
	case "integer":
		return p.parseIntegerElement(element)
	case "real":
		return p.parseRealElement(element)
	case "true", "false": // small enough to inline
		b := element.Name.Local == "true"
		p.skip() // skip the closing tag
		return cfBoolean(b)
	case "date":
		return p.parseDateElement(element)
	case "data":
		return p.parseDataElement(element)
	case "dict":
		return p.parseDictionary(element)
	case "array":
		return p.parseArray(element)
	default:
		p.unexpected(element)
		return nil
	}
}

func newXMLPlistParser(r io.Reader) *xmlPlistParser {
	return &xmlPlistParser{r, xml.NewDecoder(r)} //, strings.NewReplacer("\t", "", "\n", "", " ", "", "\r", "")}
}
