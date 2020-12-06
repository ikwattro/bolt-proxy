package bolt

import (
	"bytes"
	"testing"
)

type TestBuffer struct {
	b *bytes.Buffer
}

func NewTestBuffer(buf []byte) TestBuffer {
	return TestBuffer{bytes.NewBuffer(buf)}
}

func (t TestBuffer) Close() error {
	return nil
}

func (t TestBuffer) Read(buf []byte) (int, error) {
	return t.b.Read(buf)
}

func (t TestBuffer) Write(buf []byte) (int, error) {
	return t.b.Write(buf)
}

func TestReadMessage(t *testing.T) {
	recordData := []byte{0x0, 0x4, 0xb1, 0x71, 0x91, 0x1, 0x0, 0x0}
	conn := NewDirectConn(NewTestBuffer(recordData))

	message, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if message.T != RecordMsg {
		t.Fatalf("expected RecordMsg, got %s\n", message.T)
	}
	if !bytes.Equal(message.Data, recordData) {
		t.Fatalf("expected bytes to match input, got %#v\n", message.Data)
	}
}
