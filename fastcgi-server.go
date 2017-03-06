// Specification:
// https://web.archive.org/web/20150420080736/http://www.fastcgi.com/drupal/node/6?q=node/22
package main

import (
	"fmt"
	"io"
	"net"
	"os"
)

func main() {
	ln, err := net.Listen("tcp", ":9901")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to listen: %s\n", err)
		os.Exit(1)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Accept error: %s\n", err)
			continue
		}

		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	fmt.Printf("new connection from %s\n", conn.RemoteAddr())

	// Track whether we should close the connection after responding to a request.
	// RequestID -> bool whether to close.
	closeAfterRequest := map[int16]bool{}

	for {
		record, err := readRecord(conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading record: %s\n", err)
			break
		}

		if record.RequestID == 0 {
			fmt.Printf("Received management record.\n")
		} else {
			fmt.Printf("Received application record (request ID %d).\n",
				record.RequestID)
		}

		if record.Type == FCGIBeginRequest {
			beginRequest, err := parseBeginRequest(record)
			if err != nil {
				fmt.Fprintf(os.Stderr, "reading begin request: %s\n", err)
				break
			}

			if beginRequest.Role != FCGIResponder {
				fmt.Fprintf(os.Stderr, "unexpected role requested: %d\n",
					beginRequest.Role)
				break
			}

			fmt.Printf("received begin request with role responder\n")
			closeAfterRequest[record.RequestID] = beginRequest.Flags&0x01 == 0x01
			continue
		}

		if record.Type == FCGIParams {
			if err := parseParams(record); err != nil {
				fmt.Fprintf(os.Stderr, "reading params: %s\n", err)
				break
			}

			fmt.Printf("received params record\n")
			continue
		}

		if record.Type == FCGIStdin {
			if err := parseStdin(record); err != nil {
				fmt.Fprintf(os.Stderr, "reading stdin: %s\n", err)
				break
			}

			fmt.Printf("received stdin record\n")

			// Once we see stdin we can send our response as stdout stream
			if err := sendResponse(conn, record.RequestID); err != nil {
				fmt.Fprintf(os.Stderr, "sending response: %s\n", err)
				break
			}

			fmt.Printf("sent response\n")

			if closeAfterRequest[record.RequestID] {
				fmt.Printf("told to close connection\n")
				break
			}
			fmt.Printf("keeping connection open\n")
			delete(closeAfterRequest, record.RequestID)

			continue
		}

		fmt.Fprintf(os.Stderr, "unhandled record type: %d #%v\n", record.Type,
			record)
		break
	}

	if err := conn.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close error: %s\n", err)
		return
	}

	fmt.Printf("connection closed: %s\n", conn.RemoteAddr())
}

// Record holds a FastCGI record. See section 3.3 in the specification.
type Record struct {
	Version       int8
	Type          RecordType
	RequestID     int16
	ContentLength int16
	PaddingLength int8
	Reserved      int8
	ContentData   []byte
	PaddingData   []byte
}

// RecordType is one of the FastCGI record types.
type RecordType int8

const (
	// FCGIBeginRequest is a type component of FCGI_Header
	FCGIBeginRequest RecordType = 1
	// FCGIAbortRequest is a type component of FCGI_Header
	FCGIAbortRequest = 2
	// FCGIEndRequest is a type component of FCGI_Header
	FCGIEndRequest = 3
	// FCGIParams is a type component of FCGI_Header
	FCGIParams = 4
	// FCGIStdin is a type component of FCGI_Header
	FCGIStdin = 5
	// FCGIStdout is a type component of FCGI_Header
	FCGIStdout = 6
	// FCGIStderr is a type component of FCGI_Header
	FCGIStderr = 7
	// FCGIData is a type component of FCGI_Header
	FCGIData = 8
	// FCGIGetValues is a type component of FCGI_Header
	FCGIGetValues = 9
	// FCGIGetValuesResult is a type component of FCGI_Header
	FCGIGetValuesResult = 10
	// FCGIUnknownType is a type component of FCGI_Header
	FCGIUnknownType = 11
)

// Read and parse a FastCGI record.
//
// See section 3.3 in the specification.
func readRecord(reader io.Reader) (*Record, error) {
	// Records have a fixed length prefix. The fields in this prefix indicate how
	// much variable data follows.
	//
	// The length comes from how many unsigned char octets are present in the
	// FCGI_Record struct. The specification also explicitly says the size (under
	// padding in sectin 3.3)
	fixedLength := 8
	header := make([]byte, fixedLength)
	if err := readFull(reader, header); err != nil {
		return nil, fmt.Errorf("reading header: %s", err)
	}

	idx := 0
	version := int8(header[idx])
	if version != 1 {
		return nil, fmt.Errorf("unexpected version: %.2x", header[idx])
	}
	idx++

	recordType := getRecordType(int8(header[idx]))
	if recordType == FCGIUnknownType {
		return nil, fmt.Errorf("unknown record type: %.2x", header[idx])
	}
	idx++

	requestID := int16(int16(header[idx])<<8) | int16(header[idx+1])
	idx += 2

	contentLength := int16(int16(header[idx])<<8) | int16(header[idx+1])
	idx += 2

	paddingLength := int8(header[idx])
	idx++

	reserved := int8(header[idx])
	if reserved != 0 {
		return nil, fmt.Errorf("reserved value is not 0: %.2x", header[idx])
	}
	idx++

	content := make([]byte, contentLength)
	if err := readFull(reader, content); err != nil {
		return nil, fmt.Errorf("reading content: %s", err)
	}

	padding := make([]byte, paddingLength)
	if err := readFull(reader, padding); err != nil {
		return nil, fmt.Errorf("reading padding: %s", err)
	}

	record := &Record{
		Version:       version,
		Type:          recordType,
		RequestID:     requestID,
		ContentLength: contentLength,
		PaddingLength: paddingLength,
		Reserved:      reserved,
		ContentData:   content,
		PaddingData:   padding,
	}

	return record, nil
}

func readFull(reader io.Reader, data []byte) error {
	n, err := reader.Read(data)
	if err != nil {
		return err
	}

	if n != len(data) {
		return fmt.Errorf("short read. read %d, wanted %d", n, len(data))
	}

	return nil
}

func getRecordType(t int8) RecordType {
	switch t {
	case 1:
		return FCGIBeginRequest
	case 2:
		return FCGIAbortRequest
	case 3:
		return FCGIEndRequest
	case 4:
		return FCGIParams
	case 5:
		return FCGIStdin
	case 6:
		return FCGIStdout
	case 7:
		return FCGIStderr
	case 8:
		return FCGIData
	case 9:
		return FCGIGetValues
	case 10:
		return FCGIGetValuesResult
	default:
		return FCGIUnknownType
	}
}

// BeginRequest holds informatino for the FCGI_BeginRequestBody struct.
type BeginRequest struct {
	Role  Role
	Flags int8
}

// Role is a FCGI Role
type Role int16

const (
	// FCGIResponder is an FCGI role
	FCGIResponder = 1
	// FCGIAuthorizer is an FCGI role
	FCGIAuthorizer = 2
	// FCGIFilter is an FCGI role
	FCGIFilter = 3
	// FCGIUnknownRole is an FCGI role
	FCGIUnknownRole = 4
)

func parseBeginRequest(record *Record) (*BeginRequest, error) {
	idx := 0

	rawRole := int16((int16(record.ContentData[idx]) << 8) |
		int16(record.ContentData[idx+1]))
	role := getRole(rawRole)
	if role == FCGIUnknownRole {
		return nil, fmt.Errorf("unknown role: %.2x %.2x", record.ContentData[idx],
			record.ContentData[idx+1])
	}
	idx += 2

	flags := int8(record.ContentData[idx])
	idx++

	return &BeginRequest{
		Role:  role,
		Flags: flags,
	}, nil
}

func getRole(r int16) Role {
	switch r {
	case 1:
		return FCGIResponder
	case 2:
		return FCGIAuthorizer
	case 3:
		return FCGIFilter
	default:
		return FCGIUnknownRole
	}
}

// Parse name-value pairs. See section 3.4.
func parseParams(record *Record) error {
	for idx := 0; idx < len(record.ContentData); {
		nameLength, newIdx := readLength(record, idx)
		idx = newIdx

		valueLength, newIdx := readLength(record, idx)
		idx = newIdx

		name := make([]byte, nameLength)
		if n := copy(name, record.ContentData[idx:idx+int(nameLength)]); n != int(nameLength) {
			return fmt.Errorf("short copy of name. got %d, wanted %d", n, nameLength)
		}
		idx += int(nameLength)

		value := make([]byte, valueLength)
		if n := copy(value, record.ContentData[idx:idx+int(valueLength)]); n != int(valueLength) {
			return fmt.Errorf("short copy of value. got %d, wanted %d", n, valueLength)
		}
		idx += int(valueLength)

		fmt.Printf("Read name-value: %s = %s\n", name, value)
	}

	return nil
}

// Read a name or value length for a name-value pair.
//
// See section 3.4.
func readLength(record *Record, idx int) (int32, int) {
	// First byte's MSB tells us how many length bytes. If it's 0 then there is
	// a single byte. Otherwise there are 4.

	if record.ContentData[idx]>>7 == 0 {
		return int32(record.ContentData[idx]), idx + 1
	}

	b0 := int32(record.ContentData[idx]&0x7f) << 24
	b1 := int32(record.ContentData[idx+1]) << 16
	b2 := int32(record.ContentData[idx+2]) << 8
	b3 := int32(record.ContentData[idx+3])

	return b0 + b1 + b2 + b3, idx + 4
}

// Stdin is a stream record. This means there can be multiple records, and they
// end with one of content length 0.
//
// Return whether the stream is done.
func parseStdin(record *Record) error {
	fmt.Printf("stdin record is length %d\n", record.ContentLength)
	return nil
}

func sendResponse(writer io.Writer, requestID int16) error {
	// Send FCGIStdout records until we've sent the entire response.

	body := "hi there"
	headers := fmt.Sprintf("Content-Type: text/plain\r\n")
	response := []byte(fmt.Sprintf("%s\r\n%s", headers, body))

	rec := Record{
		Type:        FCGIStdout,
		RequestID:   requestID,
		ContentData: response,
	}

	if err := writeAll(writer, rec.serialize()); err != nil {
		return fmt.Errorf("writing stdout: %s", err)
	}
	fmt.Printf("sent stdout record: %#v\n", rec.serialize())

	// Zero length FCGIStdout record to indicate end of stream.

	rec = Record{
		Type:        FCGIStdout,
		RequestID:   requestID,
		ContentData: []byte{},
	}

	if err := writeAll(writer, rec.serialize()); err != nil {
		return fmt.Errorf("writing stdout (end of stream): %s", err)
	}
	fmt.Printf("sent stdout record: %#v\n", rec.serialize())

	// Then send FCGIEndRequest record to indicate the end.

	buf := make([]byte, 8)

	// App status. Four bytes. Leave as zero. It's to indicate the exit status.

	// Protocol status. 1 byte. Leave as 0. This is FCGI_REQUEST_COMPLETE.

	rec = Record{
		Type:        FCGIEndRequest,
		RequestID:   requestID,
		ContentData: buf,
	}

	if err := writeAll(writer, rec.serialize()); err != nil {
		return fmt.Errorf("writing end request: %s", err)
	}
	fmt.Printf("sent endrequest record: %#v\n", rec.serialize())

	return nil
}

func (r Record) serialize() []byte {
	headerSz := 8

	buf := make([]byte, headerSz)

	idx := 0

	// Version, FCGI_VERSION_1 always.
	buf[idx] = 1
	idx++

	// Type
	buf[idx] = byte(r.Type)
	idx++

	// Request ID
	buf[idx] = byte(r.RequestID >> 8)
	buf[idx+1] = byte(r.RequestID)
	idx += 2

	// Content length
	contentLength := len(r.ContentData)
	buf[idx] = byte(contentLength >> 8)
	buf[idx+1] = byte(contentLength)
	idx += 2

	// Padding length. No padding.
	buf[idx] = 0
	buf[idx+1] = 0
	idx += 2

	// Reserved. It's already 0.

	buf = append(buf, r.ContentData...)

	return buf
}

func writeAll(writer io.Writer, buf []byte) error {
	n, err := writer.Write(buf)
	if err != nil {
		return err
	}

	if n != len(buf) {
		return fmt.Errorf("short write. wrote %d, wanted %d", n, len(buf))
	}

	return nil
}
