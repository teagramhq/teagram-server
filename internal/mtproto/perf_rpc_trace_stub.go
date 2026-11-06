//go:build !performance

package mtproto

func perfRPCEvent(*Conn, int64, string, string)      {}
func perfRPCFrameEvent(*Conn, []byte, int64, string) {}
