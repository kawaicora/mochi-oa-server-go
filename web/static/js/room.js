;(function () {
  const A = window.__admin
  if (!A) return
  const $ = (id) => document.getElementById(id)
  const R = (A.room = A.room || {})
  R.isInCall = false
  R.currentRoom = null
  R.peers = []
  R.remoteStreams = {}
  R.engine = null
  R.localStream = null
  R.callTimer = null
  R.callSeconds = 0
  R.isMuted = false
  R.isCameraOff = true
  R.isScreenSharing = false
  R.isRecording = false
  R.spectrumStyle = 'avatar'
  R.focusedKey = null
  R.micDevices = []
  R.camDevices = []
  R.outDevices = []
  R.currentMic = 'default'
  R.currentCam = 'default'
  R.showChat = false
  R.showParticipants = false
  R.showMeetingInfo = false
  R.showDevices = false
  R.chatMessages = []
  R.recorder = null
  R.recChunks = []
  R.toastTimer = null
  R.roomKind = 'video'
  R.spectrumData = null

  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])) }
  function fmt(s) { const mm = String(Math.floor(s / 60)).padStart(2, '0'); const ss = String(s % 60).padStart(2, '0'); return mm + ':' + ss }

  // ─── 房间列表 ───
  R.load = async function () {
    try {
      const ack = await A.emit('rtc:listRooms', {})
      if (ack.ok) { R.render(ack.rooms || []) }
    } catch (e) { console.warn('[room] listRooms fail', e) }
  }
  R.render = function (rooms) {
    $('room-empty').style.display = rooms.length ? 'none' : ''
    $('room-count').textContent = rooms.length
    const grid = $('room-grid')
    grid.innerHTML = ''
    const typeMap = { conf: ['会议', 'rt-conf'], dm: ['私聊通话', 'rt-dm'], group: ['群通话', 'rt-group'], perm: ['永久房间', 'rt-perm'] }
    for (const r of rooms) {
      const [tn, tc] = typeMap[r.type] || [r.type, 'rt-conf']
      const el = document.createElement('div')
      el.className = 'room-card'
      const title = r.title || (r.type === 'group' ? '群通话' : r.type === 'dm' ? '私聊通话' : r.type === 'perm' ? '永久房间' : '视频会议')
      const no = r.meetingNo ? ('会议号 ' + r.meetingNo) : (r.type === 'group' ? ('群 #' + r.groupId) : r.type === 'perm' ? '常驻 · 不自动删除' : r.id)
      const started = r.startedAt ? new Date(r.startedAt).toLocaleTimeString() : ''
      el.innerHTML =
        '<span class="room-type ' + tc + '">' + tn + '</span>' +
        '<div class="room-title">' + esc(title) + '</div>' +
        '<div class="room-meta">' + esc(no) + (r.hasPassword ? ' · 🔒' : '') + ' · 👥 ' + r.peerCount + (started ? ' · 始于 ' + started : '') + '</div>' +
        '<div class="room-join">点击加入 →</div>'
      el.onclick = () => R.open(r)
      grid.appendChild(el)
    }
  }

  // ─── 点击房间 → 直接进入（无设备选择框）───
  R.open = async function (r) {
    try {
      const ack = await A.emit('rtc:join', { roomId: r.id, password: undefined, kind: r.kind || R.roomKind })
      if (!ack.ok) { A.toast(ack.error || '加入失败'); return }
      R.enterCall(ack.room, ack.peers || [], ack.iceServers || [])
    } catch (e) { A.toast(e instanceof Error ? e.message : '加入失败') }
  }

  // ─── 频谱视频轨：头像+频谱（照抄客户端 VoiceAvatarSpectrum，4096 FFT 左右声道半圆环）/ 滚动瀑布谱（ScrollingSpectrum）───
  function spectrumVideoTrack(micStream) {
    const mic = micStream || R.localStream
    if (!mic || !mic.getAudioTracks()[0]) return null
    const micOnly = new MediaStream([mic.getAudioTracks()[0]])
    let s = null
    if (R.spectrumStyle === 'scroll') {
      const scroll = new window.ScrollingSpectrum()
      s = scroll.GetScrollingSpectrumStream(micOnly, { width: 1280, height: 720, fftSize: 8192, fps: 30 })
      if (s) s.__specStop = () => scroll.stop()
    } else {
      const av = new window.VoiceAvatarSpectrum({ fftSize: 4096, minDb: -80, maxDb: 0, multiplier: 2.0, fps: 30 })
      s = av.GetVoiceAvatarStream(micOnly, { size: 1080, avatar: A.myAvatar || undefined, nick: A.myNick || '我' })
      if (s) s.__specStop = () => { try { s.__specStop && s.__specStop() } catch (e) {} }
    }
    if (!s) return null
    const t = s.getVideoTracks()[0] || null
    if (t) t.__specStop = () => { try { s.__specStop && s.__specStop() } catch (e) {} }
    return t
  }
  async function getAudioStream(deviceId) {
    return navigator.mediaDevices.getUserMedia({ audio: deviceId && deviceId !== 'default' ? { deviceId: { exact: deviceId } } : true })
  }
  async function getCameraStream(deviceId) {
    const opts = { video: { width: 1280, height: 720 } }
    if (deviceId && deviceId !== 'default') opts.video.deviceId = { exact: deviceId }
    return navigator.mediaDevices.getUserMedia(opts)
  }
  async function getScreenStream() {
    try { return await navigator.mediaDevices.getDisplayMedia({ video: true, audio: true }) }
    catch { return navigator.mediaDevices.getDisplayMedia({ video: true }) }
  }
  function updateLocalVideo(track) {
    const oldV = R.localStream.getVideoTracks()
    oldV.forEach((t) => { if (t.__specStop) { try { t.__specStop() } catch (e) {} } try { t.stop() } catch (e) {} })
    const arr = R.localStream.getAudioTracks().slice()
    if (track) arr.push(track)
    R.localStream = new MediaStream(arr)
    R.engine && R.engine.replaceTrack(track, 'video')
    R.renderGrid()
  }
  function updateLocalAudio(track) {
    const video = R.localStream.getVideoTracks().slice()
    const arr = video.slice()
    if (track) arr.push(track)
    R.localStream = new MediaStream(arr)
    R.engine && R.engine.replaceTrack(track, 'audio')
  }

  // ─── RtcEngine（在 static/js/rtc.js 中，照抄客户端）───


  // ─── 进入房间 ───
  R.enterCall = async function (room, roomPeers, iceServers) {
    R.currentRoom = room
    R.peers = roomPeers || []
    R.remoteStreams = {}
    R.isInCall = true
    $('room-hall').style.display = 'none'
    $('room-call').style.display = ''
    const no = room.meetingNo || room.id
    $('call-no').textContent = '会议号：' + no
    $('mi-no').textContent = no
    $('mi-title').textContent = room.title || '视频会议'
    $('mi-status').textContent = '已连接'
    $('who-am-i').textContent = '我：' + A.myNick

    R.localStream = new MediaStream()
    let mic = null
    try { mic = await navigator.mediaDevices.getUserMedia({ audio: true }) } catch (e) { mic = null }
    if (mic && mic.getAudioTracks()[0]) R.localStream.addTrack(mic.getAudioTracks()[0])
    const spec = spectrumVideoTrack()
    if (spec) R.localStream.addTrack(spec)
    R.isCameraOff = true
    R.isScreenSharing = false

    R.renderGrid()
    R.engine = new RtcEngine({
      roomId: room.id, iceServers, localStream: R.localStream,
      onSignal: (sig) => A.sock.emit('rtc:signal', { roomId: room.id, signal: sig }),
      onRemote: (userId, s) => { R.remoteStreams[userId] = s; R.renderGrid() },
      onDisconnect: (userId) => { delete R.remoteStreams[userId]; R.peers = R.peers.filter((p) => p.userId !== userId); R.renderGrid() }
    })
    roomPeers.forEach((p) => R.engine.addPeer(p.userId))

    R.callSeconds = 0; $('call-timer').textContent = '00:00'
    if (R.callTimer) clearInterval(R.callTimer)
    R.callTimer = setInterval(() => { R.callSeconds++; $('call-timer').textContent = fmt(R.callSeconds) }, 1000)
    R.isMuted = false
    R.refreshBar()
    R.closeAllPanels()
  }

  // ─── 九宫格 ───
  function hasVideo(s) { const t = s && s.getVideoTracks()[0]; return !!t && t.readyState !== 'ended' }
  function makeFace(nick, avatar, size, cls) {
    const d = document.createElement('div'); d.className = cls || 'tile-avatar'
    const f = document.createElement('div'); f.className = 'avatar-face'; f.style.width = size + 'px'; f.style.height = size + 'px'
    if (avatar) { const im = document.createElement('img'); im.src = avatar; im.onerror = () => { f.innerHTML = ''; f.textContent = (nick || '?').charAt(0) }; f.appendChild(im) }
    else f.textContent = (nick || '?').charAt(0)
    d.appendChild(f)
    return d
  }
  R.renderGrid = function () {
    const grid = $('call-grid')
    grid.innerHTML = ''
    const me = document.createElement('div'); me.className = 'tile' + (R.focusedKey === 'me' ? ' focused' : '')
    if (hasVideo(R.localStream)) { const v = document.createElement('video'); v.autoplay = true; v.muted = true; v.playsInline = true; v.srcObject = R.localStream; me.appendChild(v) }
    else me.appendChild(makeFace(A.myNick, A.myAvatar, 96))
    const mn = document.createElement('div'); mn.className = 'tile-name'; mn.textContent = '我：' + A.myNick; me.appendChild(mn)
    const mfs = document.createElement('button'); mfs.className = 'tile-fs'; mfs.title = '全屏'; mfs.textContent = '⛶'; mfs.onclick = (ev) => { ev.stopPropagation(); fullscreenEl(me) }; me.appendChild(mfs)
    me.onclick = () => { R.focusedKey = 'me'; R.renderGrid(); R.showFocus('me') }
    me.oncontextmenu = (ev) => { ev.preventDefault(); R.toggleSpecMenu() }
    grid.appendChild(me)
    R.peers.forEach((p) => {
      const t = document.createElement('div'); t.className = 'tile' + (R.focusedKey === p.socketId ? ' focused' : '')
      const rs = R.remoteStreams[p.userId]
      if (hasVideo(rs)) { const v = document.createElement('video'); v.autoplay = true; v.playsInline = true; v.srcObject = rs; t.appendChild(v) }
      else t.appendChild(makeFace(p.nick, p.avatar, 96))
      const n = document.createElement('div'); n.className = 'tile-name'; n.textContent = p.nick; t.appendChild(n)
      const fs = document.createElement('button'); fs.className = 'tile-fs'; fs.title = '全屏'; fs.textContent = '⛶'; fs.onclick = (ev) => { ev.stopPropagation(); fullscreenEl(t) }; t.appendChild(fs)
      t.onclick = () => { R.focusedKey = p.socketId; R.renderGrid(); R.showFocus(p.socketId) }
      grid.appendChild(t)
    })
    R.renderParticipants()
  }
  R.upsertPeer = function (p) { const idx = R.peers.findIndex((x) => x.userId === p.userId); if (idx >= 0) R.peers[idx] = p; else R.peers.push(p); R.renderGrid() }
  function focusedPeer() { return R.focusedKey ? R.peers.find((p) => p.socketId === R.focusedKey) || null : null }
  R.showFocus = function (key) {
    const ov = $('focus-overlay'); const body = $('focus-body'); const name = $('focus-name')
    body.innerHTML = ''
    if (key === 'me') {
      if (hasVideo(R.localStream)) { const v = document.createElement('video'); v.autoplay = true; v.muted = true; v.playsInline = true; v.srcObject = R.localStream; body.appendChild(v) }
      else body.appendChild(makeFace(A.myNick, A.myAvatar, 180, 'focus-avatar'))
      name.textContent = '我：' + A.myNick
    } else {
      const p = focusedPeer()
      if (p) {
        if (hasVideo(R.remoteStreams[p.userId])) { const v = document.createElement('video'); v.autoplay = true; v.playsInline = true; v.srcObject = R.remoteStreams[p.userId]; body.appendChild(v) }
        else body.appendChild(makeFace(p.nick, p.avatar, 180, 'focus-avatar'))
        name.textContent = p.nick
      }
    }
    ov.style.display = ''
  }
  R.closeFocus = function () { R.focusedKey = null; $('focus-overlay').style.display = 'none'; R.renderGrid() }
  function fullscreenEl(el) { if (document.fullscreenElement === el) { document.exitFullscreen().catch(() => {}); return } if (el.requestFullscreen) el.requestFullscreen().catch(() => {}) }

  // ─── 通话控制 ───
  R.refreshBar = function () {
    $('ctrl-mute').classList.toggle('danger', R.isMuted)
    $('ctrl-mute').innerHTML = R.isMuted ? '🔇' : '🎤'
    $('ctrl-cam').classList.toggle('danger', R.isCameraOff)
    $('ctrl-cam').innerHTML = R.isCameraOff ? '🚫' : '📷'
    $('ctrl-screen').classList.toggle('sharing', R.isScreenSharing)
    $('ctrl-rec').classList.toggle('on', R.isRecording)
  }
  R.toggleMute = function () {
    R.isMuted = !R.isMuted
    const t = R.localStream && R.localStream.getAudioTracks()[0]
    if (t) t.enabled = !R.isMuted
    R.refreshBar()
  }
  R.toggleCamera = async function () {
    try {
      if (R.isCameraOff) {
        const camStream = await getCameraStream()
        const track = camStream.getVideoTracks()[0] || null
        updateLocalVideo(track); R.isCameraOff = false; R.isScreenSharing = false
      } else {
        const track = spectrumVideoTrack()
        updateLocalVideo(track); R.isCameraOff = true; R.isScreenSharing = false
      }
      R.refreshBar(); R.renderGrid()
    } catch (e) { A.toast('切换摄像头失败') }
  }
  R.toggleScreenShare = async function () {
    if (!R.engine) return
    try {
      if (R.isScreenSharing) {
        let track = null
        if (!R.isCameraOff) { const c = await getCameraStream().catch(() => null); track = c ? c.getVideoTracks()[0] || null : null }
        if (!track) track = spectrumVideoTrack()
        updateLocalVideo(track); R.isScreenSharing = false
      } else {
        const ss = await getScreenStream()
        const track = ss.getVideoTracks()[0] || null
        if (!track) throw new Error('未获取到屏幕画面')
        updateLocalVideo(track); R.isScreenSharing = true
        track.onended = () => { R.isScreenSharing = false; R.refreshBar() }
      }
      R.refreshBar(); R.renderGrid()
    } catch (e) { A.toast('屏幕共享切换失败') }
  }
  R.toggleRecording = async function () {
    if (!R.engine) return
    if (R.isRecording) { await R.stopRecording(); return }
    try {
      const micT = R.localStream && R.localStream.getAudioTracks()[0]
      const videoT = R.localStream && R.localStream.getVideoTracks()[0]
      if (!micT && !videoT) { A.toast('录制启动失败（未获取到音视频流）'); return }
      const ms = new MediaStream([...(micT ? [micT] : []), ...(videoT ? [videoT] : [])])
      R.recorder = new MediaRecorder(ms)
      R.recChunks = []
      R.recorder.ondataavailable = (e) => { if (e.data && e.data.size) R.recChunks.push(e.data) }
      R.recorder.start()
      R.isRecording = true; R.refreshBar(); A.toast('正在录制…')
    } catch (e) { A.toast('录制启动失败') }
  }
  R.stopRecording = async function () {
    if (R.recorder && R.recorder.state !== 'inactive') { R.recorder.stop(); await new Promise((res) => { R.recorder.onstop = res }) }
    R.isRecording = false; R.refreshBar()
    if (R.recChunks.length) {
      const blob = new Blob(R.recChunks, { type: 'video/webm' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url; a.download = '通话录制-' + (R.currentRoom && (R.currentRoom.meetingNo || R.currentRoom.id)) + '-' + Date.now() + '.webm'
      a.click()
      setTimeout(() => URL.revokeObjectURL(url), 5000)
      A.toast('录制已保存（webm），已在下载')
    }
    R.recorder = null; R.recChunks = []
  }
  R.leaveCall = async function (_ended) {
    if (R.recorder && R.recorder.state !== 'inactive') { try { R.recorder.stop() } catch {} }
    if (R.callTimer) { clearInterval(R.callTimer); R.callTimer = null }
    if (R.engine) { if (R.currentRoom && !_ended) A.sock.emit('rtc:leave', { roomId: R.currentRoom.id }); R.engine.closeAll(); R.engine = null }
    if (R.localStream) { R.localStream.getTracks().forEach((t) => { if (t.__specStop) { try { t.__specStop() } catch {} } t.stop() }); R.localStream = null }
    R.currentRoom = null; R.peers = []; R.remoteStreams = {}; R.isRecording = false; R.isScreenSharing = false; R.isInCall = false
    R.closeAllPanels(); $('focus-overlay').style.display = 'none'
    $('room-call').style.display = 'none'
    $('room-hall').style.display = ''
    R.load()
  }
  R.closeAllPanels = function () {
    $('chat-panel').style.display = 'none'; $('participants-panel').style.display = 'none'; $('meeting-info').style.display = 'none'; $('dev-panel').style.display = 'none'
    $('spec-menu').style.display = 'none'; $('mic-menu').style.display = 'none'; $('cam-menu').style.display = 'none'
    R.showChat = false; R.showParticipants = false; R.showMeetingInfo = false; R.showDevices = false
  }

  // ─── 面板 ───
  $('btn-info').onclick = () => { R.showMeetingInfo = !R.showMeetingInfo; $('meeting-info').style.display = R.showMeetingInfo ? '' : 'none'; if (R.showMeetingInfo) { closeSideOther() } }
  function closeSideOther() { $('chat-panel').style.display = 'none'; R.showChat = false; $('participants-panel').style.display = 'none'; R.showParticipants = false }
  $('ctrl-users').onclick = () => { R.showParticipants = !R.showParticipants; $('participants-panel').style.display = R.showParticipants ? '' : 'none'; if (R.showParticipants) { closeSideOther(); $('meeting-info').style.display = 'none'; R.showMeetingInfo = false; R.renderParticipants() } }
  R.renderParticipants = function () {
    $('participant-count').textContent = R.peers.length + 1
    const list = $('participant-list')
    list.innerHTML = ''
    const me = document.createElement('div'); me.className = 'participant-row'
    me.innerHTML = '<div class="avatar-face" style="width:32px;height:32px;font-size:13px">' + (A.myAvatar ? '<img src="' + esc(A.myAvatar) + '" style="width:100%;height:100%;object-fit:cover;border-radius:50%">' : esc((A.myNick || '我').charAt(0))) + '</div><span class="participant-name">我：' + esc(A.myNick) + '</span><span class="participant-status">主持人</span>'
    list.appendChild(me)
    R.peers.forEach((p) => {
      const r = document.createElement('div'); r.className = 'participant-row'
      r.innerHTML = '<div class="avatar-face" style="width:32px;height:32px;font-size:13px">' + (p.avatar ? '<img src="' + esc(p.avatar) + '" style="width:100%;height:100%;object-fit:cover;border-radius:50%">' : esc((p.nick || '?').charAt(0))) + '</div><span class="participant-name">' + esc(p.nick) + '</span><span class="participant-status">在线</span>'
      list.appendChild(r)
    })
  }
  $('ctrl-chat').onclick = () => { R.showChat = !R.showChat; $('chat-panel').style.display = R.showChat ? '' : 'none'; if (R.showChat) { closeSideOther(); $('meeting-info').style.display = 'none'; R.showMeetingInfo = false; scrollChat() } }
  function pushChat(nick, content, mine, avatar) { R.chatMessages.push({ nick, content, mine, avatar }); renderChat(); scrollChat() }
  function renderChat() {
    const list = $('chat-list')
    list.innerHTML = ''
    R.chatMessages.forEach((m) => {
      const d = document.createElement('div'); d.className = 'chat-msg' + (m.mine ? ' mine' : '')
      const av = m.avatar ? '<img src="' + esc(m.avatar) + '" style="width:22px;height:22px;border-radius:50%;object-fit:cover">' : '<span>' + esc((m.nick || '?').charAt(0)) + '</span>'
      d.innerHTML = '<div class="chat-head"><div class="avatar-face" style="width:22px;height:22px;font-size:11px">' + av + '</div><div class="chat-nick">' + esc(m.nick) + '</div></div><div class="chat-bubble">' + esc(m.content) + '</div>'
      list.appendChild(d)
    })
  }
  function scrollChat() { const l = $('chat-list'); l.scrollTop = l.scrollHeight }
  $('chat-send').onclick = sendChat
  $('chat-input').addEventListener('keydown', (e) => { if (e.key === 'Enter') sendChat() })
  async function sendChat() {
    const text = $('chat-input').value.trim()
    if (!text || !R.currentRoom) return
    $('chat-input').value = ''
    const ack = await A.emit('rtc:chatMessage', { roomId: R.currentRoom.id, content: text }).catch(() => ({ ok: false }))
    if (ack.ok) pushChat(A.myNick, text, true, A.myAvatar)
  }
  $('ctrl-more').onclick = async () => {
    R.showDevices = !R.showDevices; $('dev-panel').style.display = R.showDevices ? '' : 'none'
    if (R.showDevices) { closeSideOther(); $('meeting-info').style.display = 'none'; R.showMeetingInfo = false; await refreshDeviceSelects() }
  }
  async function refreshDeviceSelects() {
    try { await navigator.mediaDevices.getUserMedia({ audio: true }).catch(() => {}) } catch (e) {}
    const all = await navigator.mediaDevices.enumerateDevices().catch(() => [])
    R.micDevices = all.filter((d) => d.kind === 'audioinput')
    R.outDevices = all.filter((d) => d.kind === 'audiooutput')
    R.camDevices = all.filter((d) => d.kind === 'videoinput')
    fillSel($('dev-mic'), R.micDevices, R.currentMic, '默认麦克风')
    fillSel($('dev-out'), R.outDevices, '', '默认扬声器')
    fillSel($('dev-cam'), R.camDevices, R.currentCam, '默认摄像头')
  }
  function fillSel(sel, devices, cur, defLabel) {
    sel.innerHTML = ''
    const o = document.createElement('option'); o.value = 'default'; o.textContent = defLabel; sel.appendChild(o)
    devices.forEach((d) => { const op = document.createElement('option'); op.value = d.deviceId; op.textContent = d.label || d.deviceId; sel.appendChild(op) })
    sel.value = cur
  }
  $('dev-apply').onclick = async () => {
    try {
      const audioIn = $('dev-mic').value
      const audioOut = $('dev-out').value
      const cam = $('dev-cam').value
      if (audioIn && audioIn !== 'default') { const s = await getAudioStream(audioIn); const t = s.getAudioTracks()[0] || null; updateLocalAudio(t); R.currentMic = audioIn }
      if (cam && cam !== 'default') { const s = await getCameraStream(cam); const t = s.getVideoTracks()[0] || null; updateLocalVideo(t); R.isCameraOff = false; R.isScreenSharing = false }
      else if (cam === 'default') { const t = spectrumVideoTrack(); updateLocalVideo(t); R.isCameraOff = true; R.isScreenSharing = false }
      if (audioOut && audioOut !== 'default' && typeof HTMLVideoElement.prototype.setSinkId === 'function') { document.querySelectorAll('video').forEach((el) => { el.setSinkId(audioOut).catch(() => {}) }) }
      R.refreshBar(); R.renderGrid(); $('dev-panel').style.display = 'none'; R.showDevices = false
    } catch (e) { A.toast('设备切换失败') }
  }

  // ─── 控制栏左键 ───
  $('ctrl-mute').onclick = R.toggleMute
  $('ctrl-cam').onclick = () => R.toggleCamera()
  $('ctrl-screen').onclick = () => R.toggleScreenShare()
  $('ctrl-rec').onclick = () => R.toggleRecording()

  // ─── 右键设备菜单 ───
  $('ctrl-mute').addEventListener('contextmenu', async (ev) => { ev.preventDefault(); await openMicMenu() })
  $('ctrl-cam').addEventListener('contextmenu', async (ev) => { ev.preventDefault(); await openCamMenu() })
  async function openMicMenu() {
    try { await navigator.mediaDevices.getUserMedia({ audio: true }).catch(() => {}) } catch (e) {}
    R.micDevices = (await navigator.mediaDevices.enumerateDevices().catch(() => [])).filter((d) => d.kind === 'audioinput')
    const items = $('mic-menu-items')
    items.innerHTML = ''
    items.appendChild(devItem('default', '🎤 系统默认', R.currentMic === 'default', () => switchMic('default')))
    if (R.micDevices.length === 0) { const e = document.createElement('button'); e.className = 'dev-pop-empty'; e.disabled = true; e.textContent = '未检测到麦克风'; items.appendChild(e) }
    R.micDevices.forEach((d) => items.appendChild(devItem(d.deviceId, '🎤 ' + (d.label || d.deviceId), R.currentMic === d.deviceId, () => switchMic(d.deviceId))))
    $('mic-menu').style.display = ''
  }
  async function openCamMenu() {
    try { await navigator.mediaDevices.getUserMedia({ video: true }).catch(() => {}) } catch (e) {}
    R.camDevices = (await navigator.mediaDevices.enumerateDevices().catch(() => [])).filter((d) => d.kind === 'videoinput')
    const items = $('cam-menu-items')
    items.innerHTML = ''
    items.appendChild(devItem('default', '📷 系统默认', R.currentCam === 'default', () => switchCamera('default')))
    if (R.camDevices.length === 0) { const e = document.createElement('button'); e.className = 'dev-pop-empty'; e.disabled = true; e.textContent = '未检测到摄像头'; items.appendChild(e) }
    R.camDevices.forEach((d) => items.appendChild(devItem(d.deviceId, '📷 ' + (d.label || d.deviceId), R.currentCam === d.deviceId, () => switchCamera(d.deviceId))))
    $('cam-menu').style.display = ''
  }
  function devItem(id, label, cur, fn) {
    const b = document.createElement('button'); b.className = 'dev-pop-item' + (cur ? ' cur' : ''); b.textContent = label
    b.onclick = fn; return b
  }
  async function switchMic(deviceId) {
    $('mic-menu').style.display = 'none'
    try { const s = await getAudioStream(deviceId); const t = s.getAudioTracks()[0] || null; updateLocalAudio(t); R.currentMic = deviceId }
    catch (e) { A.toast('麦克风切换失败') }
  }
  async function switchCamera(deviceId) {
    $('cam-menu').style.display = 'none'
    try {
      let track
      if (deviceId === 'default') { track = spectrumVideoTrack(); R.isCameraOff = true; R.isScreenSharing = false }
      else { const s = await getCameraStream(deviceId); track = s.getVideoTracks()[0] || null; R.isCameraOff = false; R.isScreenSharing = false }
      updateLocalVideo(track); R.currentCam = deviceId
      R.refreshBar(); R.renderGrid()
    } catch (e) { A.toast('摄像头切换失败') }
  }

  // ─── 语音画面样式 ───
  let specMenuShown = false
  R.toggleSpecMenu = function () { specMenuShown = !specMenuShown; $('spec-menu').style.display = specMenuShown ? '' : 'none' }
  $('spec-avatar').onclick = () => applySpec('avatar')
  $('spec-scroll').onclick = () => applySpec('scroll')
  async function applySpec(style) {
    $('spec-menu').style.display = 'none'; specMenuShown = false
    if (R.spectrumStyle === style) return
    R.spectrumStyle = style
    if (!R.isCameraOff || R.isScreenSharing) return
    const t = spectrumVideoTrack()
    if (!t) { A.toast('切换频谱失败：未获取到音频，请允许麦克风权限后重试'); return }
    updateLocalVideo(t)
    R.renderGrid()
  }

  // ─── 其它控制栏 ───
  $('ctrl-hangup').onclick = () => { if (confirm('确定结束通话？')) R.leaveCall(false) }
  $('btn-close').onclick = () => { R.leaveCall(false) }
  $('btn-min').onclick = () => A.toast('浏览器环境不支持窗口最小化')
  $('btn-max').onclick = () => { if (document.fullscreenEnabled) { if (document.fullscreenElement) document.exitFullscreen().catch(() => {}); else document.documentElement.requestFullscreen().catch(() => {}) } else A.toast('浏览器环境不支持最大化') }
  $('btn-view').onclick = () => A.toast('视图布局：网格')
  $('ctrl-shield').onclick = () => A.toast('会议安全：已加密')
  $('ctrl-ai').onclick = () => A.toast('AI 听记：浏览器环境暂未启用')
  $('ctrl-invite').onclick = copyMeetingNo
  $('call-no').onclick = copyMeetingNo
  function copyMeetingNo() {
    const no = R.currentRoom && (R.currentRoom.meetingNo || R.currentRoom.id)
    if (no && navigator.clipboard) { navigator.clipboard.writeText(no).catch(() => {}); A.toast('会议号已复制：' + no) }
  }
  $('focus-close').onclick = R.closeFocus
  $('focus-fs').onclick = () => fullscreenEl($('focus-overlay'))
  $('focus-overlay').onclick = (ev) => { if (ev.target === ev.currentTarget) R.closeFocus() }
  document.addEventListener('click', (ev) => {
    const inMenu = ev.target.closest('.dev-pop')
    if (!inMenu) { $('spec-menu').style.display = 'none'; specMenuShown = false; $('mic-menu').style.display = 'none'; $('cam-menu').style.display = 'none' }
  })

  // 房间事件（由 admin 壳转发）
  A.room.onPeerJoined = function (d) { if (R.currentRoom && d.room === R.currentRoom.id) { R.upsertPeer(d.peer); R.engine && R.engine.addPeer(d.peer.userId) } }
  A.room.onPeerLeft = function (d) { if (R.currentRoom && d.room === R.currentRoom.id) { const p = R.peers.find((x) => x.socketId === d.peerId); if (p) { R.engine && R.engine.closePeer(p.userId); delete R.remoteStreams[p.userId]; R.peers = R.peers.filter((x) => x.socketId !== d.peerId); R.renderGrid() } } }
  A.room.onSignal = function (d) { if (R.currentRoom && d.room === R.currentRoom.id) R.engine && R.engine.handleSignal(d.from.userId, d.signal) }
  A.room.onEnded = function (d) { if (R.currentRoom && d.room === R.currentRoom.id) { A.toast('会议已结束'); R.leaveCall(true) } }
  A.room.onChatMessage = function (d) { if (R.currentRoom && d.room === R.currentRoom.id && d.from.userId !== A.myUserId) pushChat(d.from.nick || '用户', d.content, false, d.from.avatar) }

  // ─── 创建房间（会议/群聊/永久）───
  const $rc = (id) => document.getElementById(id)
  $rc('room-create-btn').onclick = () => { $rc('room-create').style.display = $rc('room-create').style.display === 'none' ? '' : 'none' }
  $rc('rc-cancel').onclick = () => { $rc('room-create').style.display = 'none' }
  $rc('rc-type').onchange = async () => {
    const t = $rc('rc-type').value
    $rc('rc-group').style.display = t === 'group' ? '' : 'none'
    if (t === 'group' && !$rc('rc-group').dataset.loaded) {
      const ack = await A.emit('group:search', {}).catch(() => ({ ok: false }))
      if (ack.ok && ack.groups && ack.groups.length) {
        const sel = $rc('rc-group'); sel.innerHTML = '<option value="">— 选择群 —</option>'
        ack.groups.forEach((g) => { const o = document.createElement('option'); o.value = g.id; o.textContent = (g.name || ('群 #' + g.id)) + (g.memberCount != null ? '（' + g.memberCount + '人）' : ''); sel.appendChild(o) })
        sel.dataset.loaded = '1'
      } else { $rc('rc-group').innerHTML = '<option value="">（无群可选）</option>' }
    }
  }
  $rc('rc-submit').onclick = async () => {
    const type = $rc('rc-type').value
    const title = $rc('rc-title').value.trim()
    const kind = $rc('rc-kind').value
    const groupId = type === 'group' ? Number($rc('rc-group').value) : undefined
    if (type === 'group' && !groupId) { A.toast('群聊房间请选择群'); return }
    const payload = { type, kind, title }
    if (type === 'group') payload.groupId = groupId
    const ack = await A.emit('rtc:createRoom', payload).catch(() => ({ ok: false, error: '创建失败' }))
    if (!ack.ok) { A.toast(ack.error || '创建失败'); return }
    A.toast('房间已创建')
    $rc('room-create').style.display = 'none'
    $rc('rc-title').value = ''
    R.load()
  }

  R.init = function () {
    if (R._inited) return
    R._inited = true
    R.load()
  }
})()
