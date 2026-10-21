// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package tlsinspect parses and verifies TLS ClientHello records on named TCP
// tunnels without terminating TLS. It extracts the SNI server_name extension
// and detects Encrypted Client Hello (ECH).
package tlsinspect

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/google/agentmesh/api"
)

const (
	// RecordTypeHandshake is the TLS record content type for Handshake (22).
	RecordTypeHandshake = 0x16
	// HandshakeTypeClientHello is the TLS handshake message type for ClientHello (1).
	HandshakeTypeClientHello = 0x01
	// ExtServerName is the TLS extension type for SNI server_name (0).
	ExtServerName = 0x0000
	// ExtEncryptedClientHello is the TLS extension type for Encrypted Client Hello (0xfe0d).
	ExtEncryptedClientHello = 0xfe0d
	// MaxRecordBytes is the maximum RFC 8446 TLS record payload length (16 KiB).
	MaxRecordBytes = 16384
)

// ClientHello holds the parsed metadata and raw wire bytes of a single TLS
// ClientHello record read from a stream.
type ClientHello struct {
	// RawRecord is the complete TLS record (5-byte header + payload) suitable
	// for replaying to the upstream server after inspection.
	RawRecord []byte
	// ServerName is the raw host_name extracted from the SNI extension, if present.
	ServerName string
	// HasECH reports whether the ClientHello includes the Encrypted Client Hello extension.
	HasECH bool
}

// ReadClientHello reads a single TLS Handshake record from r and parses it as
// a ClientHello message. Even when handshake parsing fails after the record
// bytes have been read, RawRecord is populated on the returned struct if non-nil.
func ReadClientHello(r io.Reader) (*ClientHello, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("failed to read TLS record header: %w", err)
	}
	if hdr[0] != RecordTypeHandshake {
		return nil, fmt.Errorf("expected TLS Handshake record (0x16), got 0x%02x", hdr[0])
	}
	if hdr[1] != 0x03 || hdr[2] < 0x01 || hdr[2] > 0x04 {
		return nil, fmt.Errorf("invalid TLS record version 0x%02x%02x", hdr[1], hdr[2])
	}
	recLen := int(binary.BigEndian.Uint16(hdr[3:5]))
	if recLen <= 0 || recLen > MaxRecordBytes {
		return nil, fmt.Errorf("invalid TLS record length %d", recLen)
	}
	payload := make([]byte, recLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("failed to read TLS Handshake record body: %w", err)
	}

	rawRecord := make([]byte, 5+recLen)
	copy(rawRecord[:5], hdr[:])
	copy(rawRecord[5:], payload)

	sni, hasECH, err := ParseClientHelloHandshake(payload)
	if err != nil {
		return &ClientHello{RawRecord: rawRecord}, err
	}
	return &ClientHello{
		RawRecord:  rawRecord,
		ServerName: sni,
		HasECH:     hasECH,
	}, nil
}

// VerifyClientHello reads a single TLS record from r, verifies that it is an
// unencrypted TLS ClientHello whose SNI matches expectedHost and that
// Encrypted Client Hello (ECH, 0xfe0d) is not present, and returns the exact
// raw record bytes so the caller can replay them to the upstream server before
// splicing.
func VerifyClientHello(r io.Reader, expectedHost string) (rawRecord []byte, normalizedSNI string, err error) {
	ch, err := ReadClientHello(r)
	if ch != nil {
		rawRecord = ch.RawRecord
	}
	if err != nil {
		return rawRecord, "", err
	}
	if ch.HasECH {
		return rawRecord, ch.ServerName, errors.New("TLS ClientHello contains Encrypted Client Hello (ECH), which is forbidden on named TCP tunnels")
	}
	if ch.ServerName == "" {
		return rawRecord, "", errors.New("TLS ClientHello is missing SNI server_name extension")
	}
	normSNI := api.NormalizeMeshHost(ch.ServerName)
	normExpected := api.NormalizeMeshHost(expectedHost)
	if normSNI != normExpected {
		return rawRecord, ch.ServerName, fmt.Errorf("TLS ClientHello SNI %q does not match destination %q", ch.ServerName, expectedHost)
	}
	return rawRecord, normSNI, nil
}

// ParseClientHelloHandshake parses the payload of a TLS Handshake record containing
// a single ClientHello message and extracts the SNI host_name and ECH presence.
func ParseClientHelloHandshake(b []byte) (sni string, hasECH bool, err error) {
	if len(b) < 4 {
		return "", false, errors.New("truncated TLS handshake message")
	}
	if b[0] != HandshakeTypeClientHello {
		return "", false, fmt.Errorf("expected TLS ClientHello (0x01), got 0x%02x", b[0])
	}
	hsLen := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	b = b[4:]
	if len(b) < hsLen {
		return "", false, errors.New("TLS ClientHello record shorter than handshake length")
	}
	if len(b) > hsLen {
		return "", false, errors.New("TLS ClientHello record contains trailing bytes or multiple handshake messages")
	}
	b = b[:hsLen]

	// legacy_version (2) + random (32)
	if len(b) < 34 {
		return "", false, errors.New("truncated TLS ClientHello fixed header")
	}
	b = b[34:]

	// legacy_session_id (1-byte length)
	if len(b) < 1 {
		return "", false, errors.New("truncated TLS ClientHello session_id")
	}
	sidLen := int(b[0])
	b = b[1:]
	if len(b) < sidLen {
		return "", false, errors.New("truncated TLS ClientHello session_id bytes")
	}
	b = b[sidLen:]

	// cipher_suites (2-byte length)
	if len(b) < 2 {
		return "", false, errors.New("truncated TLS ClientHello cipher_suites")
	}
	csLen := int(binary.BigEndian.Uint16(b[:2]))
	b = b[2:]
	if csLen == 0 || csLen%2 != 0 || len(b) < csLen {
		return "", false, errors.New("invalid TLS ClientHello cipher_suites length")
	}
	b = b[csLen:]

	// legacy_compression_methods (1-byte length)
	if len(b) < 1 {
		return "", false, errors.New("truncated TLS ClientHello compression_methods")
	}
	compLen := int(b[0])
	b = b[1:]
	if compLen == 0 || len(b) < compLen {
		return "", false, errors.New("invalid TLS ClientHello compression_methods length")
	}
	b = b[compLen:]

	if len(b) == 0 {
		return "", false, nil
	}
	if len(b) < 2 {
		return "", false, errors.New("truncated TLS ClientHello extensions length")
	}
	extTotalLen := int(binary.BigEndian.Uint16(b[:2]))
	b = b[2:]
	if len(b) != extTotalLen {
		return "", false, errors.New("invalid TLS ClientHello extensions block length")
	}

	seenSNIExt := false
	for len(b) >= 4 {
		extType := binary.BigEndian.Uint16(b[:2])
		extLen := int(binary.BigEndian.Uint16(b[2:4]))
		b = b[4:]
		if len(b) < extLen {
			return "", false, errors.New("truncated TLS extension data")
		}
		extData := b[:extLen]
		b = b[extLen:]

		switch extType {
		case ExtEncryptedClientHello:
			hasECH = true
		case ExtServerName:
			if seenSNIExt {
				return "", false, errors.New("duplicate server_name extension in TLS ClientHello")
			}
			seenSNIExt = true
			parsed, err := parseServerNameExtension(extData)
			if err != nil {
				return "", false, err
			}
			sni = parsed
		}
	}
	if len(b) != 0 {
		return "", false, errors.New("trailing bytes in TLS ClientHello extensions")
	}
	return sni, hasECH, nil
}

func parseServerNameExtension(b []byte) (string, error) {
	if len(b) < 2 {
		return "", errors.New("truncated server_name extension")
	}
	listLen := int(binary.BigEndian.Uint16(b[:2]))
	b = b[2:]
	if len(b) != listLen {
		return "", errors.New("invalid server_name list length")
	}
	var hostName string
	for len(b) >= 3 {
		nameType := b[0]
		nameLen := int(binary.BigEndian.Uint16(b[1:3]))
		b = b[3:]
		if len(b) < nameLen || nameLen == 0 {
			return "", errors.New("invalid server_name entry length")
		}
		val := string(b[:nameLen])
		b = b[nameLen:]
		if nameType == 0x00 {
			if hostName != "" {
				return "", errors.New("duplicate host_name entry in server_name extension")
			}
			hostName = val
		}
	}
	if len(b) != 0 {
		return "", errors.New("trailing bytes in server_name list")
	}
	return hostName, nil
}
