// 回归测试（D1）：连接成功后客户端主动 socket.disconnect()，
// 服务端 readLoopWS 不得因收到 socket.io DISCONNECT 后继续读 nil ws 而 panic。
// 用法: node disconnect_client.js <port>
const { io } = require('socket.io-client');
const port = process.argv[2];

const socket = io(`http://localhost:${port}`, {
  transports: ['polling', 'websocket'],
  auth: { token: 'test-token', device: 'disconnect-regress' },
  reconnection: false,
  timeout: 8000,
});

let done = false;
setTimeout(() => {
  if (!done) { console.error('TIMEOUT'); process.exit(1); }
}, 8000);

socket.on('connect', () => {
  console.log('CONNECTED sid=' + socket.id + ' transport=' + socket.io.engine.transport.name);
  // 等 transport 升级稳定后主动断开，触发服务端收到 socket.io DISCONNECT。
  setTimeout(() => {
    socket.disconnect();
    console.log('CLIENT_DISCONNECT_CALLED');
  }, 300);
});

socket.on('disconnect', (reason) => {
  console.log('DISCONNECT_RECEIVED reason=' + reason);
  done = true;
  // 留一点时间给服务端读循环处理 DISCONNECT，若服务端 panic 本进程会随之崩溃。
  setTimeout(() => { console.log('DISCONNECT_OK'); process.exit(0); }, 500);
});

socket.on('connect_error', (err) => {
  console.error('CONNECT_ERROR ' + err.message);
  process.exit(1);
});
