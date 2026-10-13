/**
 * /view/admin 壳逻辑（Flask 式静态资源）。
 * 依赖：/view/admin/config.js（服务端动态下发 window.__ADMIN_CONFIG__）、socket.io。
 */
;(function () {
  const CONFIG = window.__ADMIN_CONFIG__ || { path: '/socket.io', transports: ['websocket', 'polling'] }
  const $ = (id) => document.getElementById(id)
  const A = (window.__admin = {
    sock: null,
    token: localStorage.getItem('vr_token') || '',
    myUser: null,
    myNick: '我',
    myAvatar: '',
    myUserId: 0,
    currentPage: null
  })

  A.esc = function (s) { return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])) }
  A.emit = function (evt, payload) {
    return new Promise((resolve) => {
      if (!A.sock) return resolve({ ok: false, error: '未连接' })
      A.sock.emit(evt, payload, (res) => resolve(res || { ok: false, error: '无响应' }))
      setTimeout(() => resolve({ ok: false, error: '请求超时' }), 15000)
    })
  }
  let toastTimer = null
  A.toast = function (msg) {
    const t = $('rec-toast')
    if (!t) { alert(msg); return }
    t.textContent = msg; t.style.display = ''
    if (toastTimer) clearTimeout(toastTimer)
    toastTimer = setTimeout(() => { t.style.display = 'none' }, 3000)
  }

  function connect() {
    if (A.sock) { A.sock.disconnect(); A.sock = null }
    A.sock = io(location.origin, { path: CONFIG.path, transports: CONFIG.transports, auth: A.token ? { token: A.token, device: 'web-admin' } : { device: 'web-admin' } })
    A.sock.on('connect_error', (e) => { console.warn('[admin] connect_error', e && e.message) })
    A.sock.on('disconnect', (r) => { console.warn('[admin] disconnect', r); if (!A.token) showLogin() })
    A.sock.on('auth:sessionRevoked', () => { A.token = ''; localStorage.removeItem('vr_token'); showLogin() })
    A.sock.on('rtc:roomsChanged', () => { if (A.currentPage === 'room' && A.room) A.room.load() })
    A.sock.on('rtc:peerJoined', (d) => { A.room && A.room.onPeerJoined && A.room.onPeerJoined(d) })
    A.sock.on('rtc:peerLeft', (d) => { A.room && A.room.onPeerLeft && A.room.onPeerLeft(d) })
    A.sock.on('rtc:signal', (d) => { A.room && A.room.onSignal && A.room.onSignal(d) })
    A.sock.on('rtc:ended', (d) => { A.room && A.room.onEnded && A.room.onEnded(d) })
    A.sock.on('rtc:chatMessage', (d) => { A.room && A.room.onChatMessage && A.room.onChatMessage(d) })
  }

  // ═══ 视图切换 ═══
  function showLogin() { $('view-login').style.display = ''; $('view-noperm').style.display = 'none'; $('view-admin').style.display = 'none' }
  function showNoPerm() { $('view-login').style.display = 'none'; $('view-noperm').style.display = ''; $('view-admin').style.display = 'none' }
  function showAdmin() { $('view-login').style.display = 'none'; $('view-noperm').style.display = 'none'; $('view-admin').style.display = ''; $('side-user').textContent = A.myNick }
  function go(page) {
    if (page !== 'room' && A.room && A.room.isInCall) { try { A.room.leaveCall(false) } catch (e) {} }
    A.currentPage = page
    ;['room', 'sysmon', 'users', 'companies'].forEach((p) => { $('page-' + p).style.display = p === page ? '' : 'none' })
    document.querySelectorAll('.side-item[data-page]').forEach((b) => b.classList.toggle('on', b.getAttribute('data-page') === page))
    if (page === 'room') A.room && A.room.init()
    if (page === 'sysmon') A.sysmon && A.sysmon.init()
    if (page === 'users') A.users && A.users.init()
    if (page === 'companies') A.companies && A.companies.init()
  }
  A.go = go

  // ═══ 登录 ═══
  async function doLogin() {
    const account = $('login-account').value.trim()
    const password = $('login-pwd').value
    $('login-err').style.display = 'none'
    if (!account || !password) { showErr('请输入用户名和密码'); return }
    try {
      connect()
      await new Promise((res, rej) => { A.sock.once('connect', res); A.sock.once('connect_error', rej); setTimeout(() => rej(new Error('连接超时')), 8000) })
      const ack = await A.emit('auth:login', { account, password, device: 'web-admin' })
      if (!ack.ok) { showErr(ack.error || '登录失败'); return }
      if (!ack.serverAdmin) { A.token = ''; localStorage.removeItem('vr_token'); showNoPerm(); return }
      A.token = ack.token; localStorage.setItem('vr_token', A.token)
      A.myUser = ack.user; A.myNick = ack.user.nick || ack.user.username; A.myAvatar = ack.user.avatar || ''; A.myUserId = ack.user.id
      showAdmin(); go('room')
    } catch (e) { showErr(e instanceof Error ? e.message : '登录失败') }
  }
  function showErr(m) { $('login-err').textContent = m; $('login-err').style.display = '' }

  $('login-btn').onclick = doLogin
  $('login-pwd').addEventListener('keydown', (e) => { if (e.key === 'Enter') doLogin() })
  $('login-account').addEventListener('keydown', (e) => { if (e.key === 'Enter') $('login-pwd').focus() })
  $('side-logout').onclick = () => { A.token = ''; localStorage.removeItem('vr_token'); if (A.sock) A.sock.disconnect(); showLogin() }
  $('noperm-back').onclick = () => { showLogin() }
  document.querySelectorAll('.side-item[data-page]').forEach((b) => { b.onclick = () => go(b.getAttribute('data-page')) })

  ;(async function init() {
    const q = new URLSearchParams(location.search)
    const urlToken = q.get('token') || ''
    if (urlToken) { A.token = urlToken; localStorage.setItem('vr_token', urlToken) }
    if (A.token) {
      try {
        connect()
        await new Promise((res, rej) => { A.sock.once('connect', res); A.sock.once('connect_error', rej); setTimeout(() => rej(new Error('连接超时')), 8000) })
        const ack = await A.emit('auth:me', {})
        if (ack.ok) {
          if (!ack.serverAdmin) { A.token = ''; localStorage.removeItem('vr_token'); showNoPerm(); return }
          A.myUser = ack.user; A.myNick = ack.user.nick || ack.user.username; A.myAvatar = ack.user.avatar || ''; A.myUserId = ack.user.id
          showAdmin(); go('room'); return
        }
        A.token = ''; localStorage.removeItem('vr_token'); showLogin()
      } catch { showLogin() }
    } else { showLogin() }
  })()
})()
