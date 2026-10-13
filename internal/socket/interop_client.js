// 真实 socket.io-client 互操作验证脚本（由 Go 测试拉起，连接本地 Go 服务端）。
// 用法: node interop_client.js <port>
const { io } = require('socket.io-client');
const port = process.argv[2];

const socket = io(`http://localhost:${port}`, {
  transports: ['polling', 'websocket'],
  auth: { token: 'test-token', device: 'interop-test' },
  reconnection: false,
  timeout: 8000,
});

let done = false;
setTimeout(() => {
  if (!done) { console.error('TIMEOUT'); process.exit(1); }
}, 8000);

socket.on('connect', () => {
  console.log('CONNECTED sid transport=' + socket.io.engine.transport.name);
  // 发一个带 ack 的事件
  socket.emit('echo:ping', { msg: 'hello-from-client' }, (res) => {
    console.log('ACK_RECEIVED ' + JSON.stringify(res));
    if (res && res.ok === true && res.echo === 'hello-from-client') {
      console.log('INTEROP_OK');
      done = true;
      process.exit(0);
    } else {
      console.error('BAD_ACK ' + JSON.stringify(res));
      process.exit(1);
    }
  });
});

socket.on('connect_error', (err) => {
  console.error('CONNECT_ERROR ' + err.message);
  process.exit(1);
});

socket.on('disconnect', (reason) => {
  console.log('DISCONNECT reason=' + reason);
});
