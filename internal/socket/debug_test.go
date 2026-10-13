package socket
import ("testing"; "fmt")
func TestDebugEncode(t *testing.T){
  b := encodeEIOOPEN("abc123", []string{"websocket"}, 25000, 20000, maxHttpBufferSize)
  fmt.Printf("OPEN bytes len=%d content=%s\n", len(b), string(b))
}
