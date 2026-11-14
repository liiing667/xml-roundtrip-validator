package validator

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
)

// XMLRoundtripError is returned when a round-trip token doesn't match the original
type XMLRoundtripError struct {
	Expected, Observed xml.Token
	Overflow           []byte
}

func (err XMLRoundtripError) Error() string {
	if len(err.Overflow) == 0 {
		return fmt.Sprintf("roundtrip error: expected %v, observed %v", err.Expected, err.Observed)
	}
	return fmt.Sprintf("roundtrip error: unexpected overflow after token: %s", err.Overflow)
}

// XMLValidationError is returned when validating an XML document fails
type XMLValidationError struct {
	Start, End, Line, Column int64
	err                      error
}

func (err XMLValidationError) Error() string {
	return fmt.Sprintf("validator: in token starting at %d:%d: %s", err.Line, err.Column, err.err.Error())
}

func (err XMLValidationError) Unwrap() error {
	return err.err
}

// Validate makes sure the given XML bytes survive round trips through encoding/xml without mutations
func Validate(xmlReader io.Reader) error {
	xmlBuffer := &bytes.Buffer{}
	xmlReader = &byteReader{io.TeeReader(xmlReader, xmlBuffer)}
	decoder := xml.NewDecoder(xmlReader)
	decoder.Strict = false
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) { return input, nil }
	offset := int64(0)
	for {
		token, err := decoder.RawToken()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if err := CheckToken(token); err != nil {
			xmlBytes := xmlBuffer.Bytes()
			line := bytes.Count(xmlBytes[0:offset], []byte{'\n'}) + 1
			lineStart := int64(bytes.LastIndexByte(xmlBytes[0:offset], '\n')) + 1
			column := offset - lineStart + 1
			return XMLValidationError{
				Start:  offset,
				End:    decoder.InputOffset(),
				Line:   int64(line),
				Column: column,
				err:    err,
			}
		}
		offset = decoder.InputOffset()
	}
}

// ValidateAll is like Validate, but instead of returning after the first error,
// it accumulates errors and validates the entire document
func ValidateAll(xmlReader io.Reader) []error {
	xmlBuffer := &bytes.Buffer{}
	xmlReader = io.TeeReader(xmlReader, xmlBuffer)
	errs := []error{}
	offset := int64(0)
	line := int64(1)
	column := int64(1)
	for {
		err := Validate(xmlReader)
		if err == nil {
			// reached the end with no additional errors
			break
		}
		validationError := XMLValidationError{}
		if errors.As(err, &validationError) {
			// validation errors contain line numbers and offsets, but
			// these offsets are based on the offset where Validate
			// was called, so they need to be adjusted to accordingly
			validationError.Start += offset
			validationError.End += offset
			if validationError.Line == 1 {
				validationError.Column += column - 1
			}
			validationError.Line += line - 1
			errs = append(errs, validationError)
			xmlBytes := xmlBuffer.Bytes()
			offset += int64(len(xmlBytes))
			newLines := int64(bytes.Count(xmlBytes, []byte("\n")))
			line += newLines
			if newLines > 0 {
				column = int64(len(xmlBytes) - bytes.LastIndex(xmlBytes, []byte("\n")))
			} else {
				column += int64(len(xmlBytes))
			}
			xmlBuffer.Reset()
		} else {
			// this was not a validation error, but likely
			// completely unparseable XML instead; no point
			// in trying to continue
			errs = append(errs, err)
			break
		}
	}
	return errs
}

// bufio implements a ByteReader but we explicitly don't want any buffering
type byteReader struct {
	r io.Reader
}

func (r *byteReader) ReadByte() (byte, error) {
	var p [1]byte
	n, err := r.r.Read(p[:])

	// The doc for the io.ByteReader interface states:
	//   If ReadByte returns an error, no input byte was consumed, and the returned byte value is undefined.
	// So if a byte is actually extracted from the reader, and we want to return it, we mustn't return the error.
	if n > 0 {
		// this byteReader is only used in the context of the Validate() function,
		// we deliberately choose to completely ignore the error in this case.
		// return the byte extracted from the reader
		return p[0], nil
	}

	return 0, err
}

func (r *byteReader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

// CheckToken computes a round trip for a given xml.Token and returns an
// error if the newly calculated token differs from the original
func CheckToken(before xml.Token) error {
	buffer := &bytes.Buffer{}
	encoder := xml.NewEncoder(buffer)

	switch t := before.(type) { // nolint:gocritic
	case xml.EndElement:
		// xml.Encoder expects matching StartElements for all EndElements
		if err := encoder.EncodeToken(xml.StartElement{Name: t.Name}); err != nil {
			return err
		}
	}

	if err := encoder.EncodeToken(before); err != nil {
		return err
	}
	if err := encoder.Flush(); err != nil {
		return err
	}
	encoded := buffer.Bytes()
	decoder := xml.NewDecoder(bytes.NewReader(encoded))
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) { return input, nil }

	switch before.(type) { // nolint:gocritic
	case xml.EndElement:
		if _, err := decoder.RawToken(); err != nil {
			return err
		}
	}

	after, err := decoder.RawToken()
	if err != nil {
		return err
	}

	if !tokenEquals(before, after) {
		return XMLRoundtripError{before, after, nil}
	}
	offset := decoder.InputOffset()
	if offset != int64(len(encoded)) {
		// this is likely unreachable, but just in case
		return XMLRoundtripError{before, after, encoded[offset:]}
	}
	return nil
}

func tokenEquals(before, after xml.Token) bool {
	switch t1 := before.(type) {
	case xml.CharData:
		t2, ok := after.(xml.CharData)
		if !ok {
			return false
		}
		return bytes.Equal(t1, t2)

	case xml.Comment:
		t2, ok := after.(xml.Comment)
		if !ok {
			return false
		}
		return bytes.Equal(t1, t2)

	case xml.Directive:
		t2, ok := after.(xml.Directive)
		if !ok {
			return false
		}
		return bytes.Equal(t1, t2)

	case xml.EndElement:
		t2, ok := after.(xml.EndElement)
		if !ok {
			return false
		}
		// local name should equal; namespace prefixes get erased
		return t1.Name.Local == t2.Name.Local && t2.Name.Space == ""

	case xml.ProcInst:
		t2, ok := after.(xml.ProcInst)
		if !ok {
			return false
		}
		return t1.Target == t2.Target && bytes.Equal(t1.Inst, t2.Inst)

	case xml.StartElement:
		t2, ok := after.(xml.StartElement)
		if !ok {
			return false
		}
		return startElementsEqual(t1, t2)
	}
	return false
}

// xmlNamespace is the namespace URI bound to the reserved "xml" prefix
const xmlNamespace = "http://www.w3.org/XML/1998/namespace"

// startElementsEqual compares two start elements in a namespace-aware manner.
// The encoder rewrites namespaces deterministically: an element prefix turns
// into a leading default xmlns attribute holding the prefix as its value, and
// every attribute prefix is replaced by a generated prefix declared through an
// xmlns attribute emitted right before the first attribute using it. Instead
// of guessing prefixes, this function resolves those declarations back to the
// namespaces they bind and compares URI and local name of the tag and of every
// attribute, preserving prefix identity and attribute order.
func startElementsEqual(before, after xml.StartElement) bool {
	if before.Name == after.Name && attrsEqual(before.Attr, after.Attr) {
		return true
	}
	if before.Name.Local != after.Name.Local {
		return false
	}
	afterAttrs := after.Attr
	switch {
	case before.Name.Space == after.Name.Space:
		// same namespace on both sides; nothing to resolve
	case before.Name.Space != "" && after.Name.Space == "":
		// an encoded element prefix shows up as a leading default xmlns
		// attribute holding the prefix as its value
		if len(afterAttrs) == 0 ||
			afterAttrs[0].Name != (xml.Name{Local: "xmlns"}) ||
			afterAttrs[0].Value != before.Name.Space {
			return false
		}
		afterAttrs = afterAttrs[1:]
	default:
		return false
	}
	resolved, ok := resolveEncodedAttrs(afterAttrs)
	if !ok {
		return false
	}
	return attrsEqual(before.Attr, resolved)
}

func attrsEqual(a, b []xml.Attr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// resolveEncodedAttrs maps the attributes of an encoded start element back to
// the namespaces their prefixes are bound to. Encoder-generated xmlns
// declarations are dropped after resolving every prefixed attribute to the
// namespace URI its prefix is bound to; all other attributes keep their
// identity and order. It returns false if the attributes cannot be the result
// of encoding a token, e.g. when a prefixed attribute has no matching
// declaration or a prefix is declared twice.
func resolveEncodedAttrs(attrs []xml.Attr) ([]xml.Attr, bool) {
	bindings := map[string]string{}
	resolved := make([]xml.Attr, 0, len(attrs))
	for i, attr := range attrs {
		if attr.Name.Space == "xmlns" && attr.Name.Local != "" &&
			i+1 < len(attrs) && attrs[i+1].Name.Space == attr.Name.Local {
			// generated declaration, emitted right before the first
			// attribute using the generated prefix
			prefix := attr.Name.Local
			if _, ok := bindings[prefix]; ok {
				return nil, false
			}
			bindings[prefix] = attr.Value
			continue
		}
		resolved = append(resolved, attr)
	}
	for i := range resolved {
		switch resolved[i].Name.Space {
		case "", "xmlns":
			// no namespace, or a namespace declaration of the original
			// token; keep the attribute as is
		case "xml":
			// the encoder uses the reserved xml prefix for the XML
			// namespace without emitting a declaration
			resolved[i].Name.Space = xmlNamespace
		default:
			namespace, ok := bindings[resolved[i].Name.Space]
			if !ok {
				return nil, false
			}
			resolved[i].Name.Space = namespace
		}
	}
	return resolved, true
}
