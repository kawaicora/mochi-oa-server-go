/**
 * RtcEngine —— WebRTC 信令引擎（照抄客户端，服务端网页版）。
 * 提供 addPeer / handleSignal / replaceTrack / closeAll 等。
 */
;(function () {
  function toIce(ices) { return (ices || []).map((i) => ({ urls: i.urls, username: i.username, credential: i.credential })) }
  function stereo(sdp) { return (sdp || '').replace(/(a=fmtp:111 .*)/g, (line) => (/stereo=1/.test(line) ? line : line + ';stereo=1;sprop-stereo=1')) }

  window.RtcEngine = class RtcEngine {
    constructor(opts) {
      this.roomId = opts.roomId
      this.ice = toIce(opts.iceServers)
      this.localStream = opts.localStream
      this.onSignal = opts.onSignal
      this.onRemote = opts.onRemote
      this.onDisconnect = opts.onDisconnect
      this.peers = new Map()
      this._pendingIce = new Map()
    }

    addPeer(userId) {
      const ex = this.peers.get(userId)
      if (ex) { if (ex.signalingState !== 'stable') { ex.close(); this.peers.delete(userId) } else return }
      const pc = new RTCPeerConnection({ iceServers: this.ice })
      this.peers.set(userId, pc)
      this.localStream.getTracks().forEach((t) => pc.addTrack(t, this.localStream))
      this._bind(pc, userId)
      this._offer(pc, userId)
    }

    handleSignal(from, sig) {
      let pc = this.peers.get(from)
      if (!pc && sig.type === 'offer') {
        pc = new RTCPeerConnection({ iceServers: this.ice })
        this.peers.set(from, pc)
        this.localStream.getTracks().forEach((t) => pc.addTrack(t, this.localStream))
        this._bind(pc, from)
      }
      if (!pc) return
      if (sig.type === 'offer') this._handleOffer(pc, from, sig.sdp)
      else if (sig.type === 'answer') this._handleAnswer(pc, from, sig.sdp)
      else if (sig.type === 'ice') this._handleIce(pc, from, sig)
    }

    replaceTrack(track, kind) {
      this.peers.forEach((pc) => {
        pc.getSenders().forEach((s) => { if (s.track && s.track.kind === kind) s.replaceTrack(track) })
        this._renegotiate(pc)
      })
    }

    closePeer(userId) { const pc = this.peers.get(userId); if (pc) { pc.close(); this.peers.delete(userId) } }
    closeAll() { this.peers.forEach((pc) => pc.close()); this.peers.clear() }

    _bind(pc, userId) {
      pc.ontrack = (e) => { if (e.streams.length) this.onRemote(userId, e.streams[0]) }
      pc.onicecandidate = (e) => { if (e.candidate) this.onSignal({ type: 'ice', candidate: e.candidate.candidate, sdpMid: e.candidate.sdpMid, sdpMLineIndex: e.candidate.sdpMLineIndex }) }
      pc.onconnectionstatechange = () => { if (pc.connectionState === 'disconnected' || pc.connectionState === 'failed' || pc.connectionState === 'closed') { this.closePeer(userId); this.onDisconnect(userId) } }
    }

    async _offer(pc, userId) {
      try { const o = await pc.createOffer(); o.sdp = stereo(o.sdp); await pc.setLocalDescription(o); this.onSignal({ type: 'offer', sdp: o.sdp || '' }) } catch (e) { console.warn('offer fail', e) }
    }
    async _handleOffer(pc, userId, sdp) {
      try {
        if (pc.signalingState === 'have-remote-offer') { try { await pc.setRemoteDescription({ type: 'rollback' }) } catch (e) {} }
        await pc.setRemoteDescription({ type: 'offer', sdp })
        await this._flushIce(userId, pc)
        const a = await pc.createAnswer(); a.sdp = stereo(a.sdp); await pc.setLocalDescription(a); this.onSignal({ type: 'answer', sdp: a.sdp || '' })
      } catch (e) { console.warn('handleOffer fail', e) }
    }
    async _handleAnswer(pc, userId, sdp) {
      try { if (pc.signalingState === 'have-local-offer') { await pc.setRemoteDescription({ type: 'answer', sdp }); await this._flushIce(userId, pc) } } catch (e) { console.warn('handleAnswer fail', e) }
    }
    _handleIce(pc, userId, sig) {
      if (!pc.remoteDescription) {
        const arr = this._pendingIce.get(userId) || (this._pendingIce.set(userId, []), this._pendingIce.get(userId))
        arr.push({ candidate: sig.candidate, sdpMid: sig.sdpMid || undefined, sdpMLineIndex: sig.sdpMLineIndex != null ? sig.sdpMLineIndex : undefined })
        return
      }
      pc.addIceCandidate(new RTCIceCandidate({ candidate: sig.candidate, sdpMid: sig.sdpMid || undefined, sdpMLineIndex: sig.sdpMLineIndex != null ? sig.sdpMLineIndex : undefined })).catch(() => {})
    }
    async _flushIce(userId, pc) {
      const arr = this._pendingIce.get(userId)
      if (!arr || !arr.length) return
      this._pendingIce.delete(userId)
      for (const c of arr) { try { await pc.addIceCandidate(new RTCIceCandidate(c)) } catch (e) {} }
    }
    async _renegotiate(pc) {
      if (pc.signalingState !== 'stable') return
      try { const o = await pc.createOffer(); o.sdp = stereo(o.sdp); await pc.setLocalDescription(o); this.onSignal({ type: 'offer', sdp: o.sdp || '' }) } catch (e) { console.warn('renegotiate fail', e) }
    }
  }
})()
