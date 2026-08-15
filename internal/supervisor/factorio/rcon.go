package factorio

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

const (
	rconAuth         int32 = 3
	rconAuthResponse int32 = 2
	rconExec         int32 = 2
	rconMaxPacket          = 4096
)

func runRCON(ctx context.Context, conn net.Conn, password, command string) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := writeRCONPacket(conn, 1, rconAuth, password); err != nil {
		return fmt.Errorf("rcon auth write: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, kind, _, err := readRCONPacket(conn)
		if err != nil {
			return fmt.Errorf("rcon auth read: %w", err)
		}
		if kind != rconAuthResponse {
			continue
		}
		if id == -1 {
			return fmt.Errorf("rcon authentication failed")
		}
		break
	}
	if err := writeRCONPacket(conn, 2, rconExec, command); err != nil {
		return fmt.Errorf("rcon command write: %w", err)
	}
	return nil
}

func writeRCONPacket(w io.Writer, id, kind int32, body string) error {
	var payload bytes.Buffer
	if err := binary.Write(&payload, binary.LittleEndian, id); err != nil {
		return err
	}
	if err := binary.Write(&payload, binary.LittleEndian, kind); err != nil {
		return err
	}
	payload.WriteString(body)
	payload.WriteByte(0)
	payload.WriteByte(0)
	if err := binary.Write(w, binary.LittleEndian, int32(payload.Len())); err != nil {
		return err
	}
	_, err := w.Write(payload.Bytes())
	return err
}

func readRCONPacket(r io.Reader) (id, kind int32, body string, err error) {
	var size int32
	if err = binary.Read(r, binary.LittleEndian, &size); err != nil {
		return 0, 0, "", err
	}
	if size < 10 || size > rconMaxPacket {
		return 0, 0, "", fmt.Errorf("invalid rcon packet size %d", size)
	}
	buf := make([]byte, size)
	if _, err = io.ReadFull(r, buf); err != nil {
		return 0, 0, "", err
	}
	id = int32(binary.LittleEndian.Uint32(buf[0:4]))
	kind = int32(binary.LittleEndian.Uint32(buf[4:8]))
	body = string(bytes.TrimRight(buf[8:], "\x00"))
	return id, kind, body, nil
}
