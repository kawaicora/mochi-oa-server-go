/**
 * RtcEngine —— 通话/会议媒体引擎（LiveKit SFU 版，浏览器 /view/admin 用）。
 *
 * 迁移背景：原 P2P 网状（按对端 userId 维护 RTCPeerConnection，走服务端 rtc:signal
 * 转发 offer/answer/ice）。现改为统一连接自建 LiveKit SFU（wss://live.kawaimoe.org），
 * 由 SFU 转发媒体：本端发布本地轨道，订阅其他参与者轨道。
 *
 * 【公开 API 保持不变】——room.js 调用方无需大改：
 *   - 构造参数：roomId / sfu{url,token,roomName} / localStream / 回调（onSignal/onRemote/onDisconnect）
 *   - addPeer / handleSignal / closePeer 变为空操作（LiveKit 自动订阅，无需 P2P 信令）
 *   - replaceTrack(track, kind) 保留（切换摄像头/麦克风/屏幕/频谱轨）
 *   - closeAll() 断开会话（必须断开，否则 Room autoReconnect 会一直后台重连）
 *   - 回调：onRemote(userId, stream) 收到某参与者媒体；onDisconnect(userId) 该参与者离开
 *
 * 依赖：全局 LivekitClient（livekit-client.umd.min.js，/view/admin 引入）。
 * 配置来源：服务端 rtc:join 等统一签发下发 sfu{url,token,roomName}（.env LIVEKIT_*），
 * 网页端不持有 apiSecret。
 */
;(function () {
  const LK = () => window.LivekitClient
  function numId(identity) {
    const n = Number(identity)
    return Number.isFinite(n) ? n : -1
  }

  window.RtcEngine = class RtcEngine {
    constructor(opts) {
      this.roomId = opts.roomId
      this.sfu = opts.sfu
      this.localStream = opts.localStream
      this.onSignal = opts.onSignal
      this.onRemote = opts.onRemote
      this.onDisconnect = opts.onDisconnect
      this.room = null
      this.remoteTracksByUser = new Map() // uid -> MediaStreamTrack[]
      this.publishedVideo = null
      this.publishedAudio = null
      this.disposed = false
      this._connect()
    }

    async _connect() {
      if (!this.sfu || !this.sfu.url || !this.sfu.token) {
        console.error('[RtcEngine] 无法获取 SFU 信息（服务端未启用 LiveKit / 未配置 LIVEKIT_*），视频链路不可用')
        return
      }
      const C = LK()
      const room = new C.Room()
      this.room = room

      // 订阅远端：每收到一条轨道，按参与者重建 MediaStream 触发 UI 重绑
      room.on(C.RoomEvent.TrackSubscribed, (track, _pub, participant) => {
        const uid = numId(participant.identity)
        if (uid < 0 || !track.mediaStreamTrack) return
        const list = this.remoteTracksByUser.get(uid) || []
        if (!list.includes(track.mediaStreamTrack)) list.push(track.mediaStreamTrack)
        this.remoteTracksByUser.set(uid, list)
        this.onRemote(uid, new MediaStream(list.slice()))
      })
      room.on(C.RoomEvent.TrackUnsubscribed, (track, _pub, participant) => {
        const uid = numId(participant.identity)
        if (uid < 0) return
        const list = (this.remoteTracksByUser.get(uid) || []).filter((t) => t !== track.mediaStreamTrack)
        if (list.length) this.remoteTracksByUser.set(uid, list)
        else this.remoteTracksByUser.delete(uid)
        this.onRemote(uid, new MediaStream(list.slice()))
      })
      room.on(C.RoomEvent.ParticipantDisconnected, (participant) => {
        const uid = numId(participant.identity)
        this.remoteTracksByUser.delete(uid)
        this.onDisconnect(uid)
      })
      room.on(C.RoomEvent.Disconnected, () => {
        console.log('[RtcEngine] LiveKit 连接断开')
      })

      try {
        await room.connect(this.sfu.url, this.sfu.token)
        // 竞态兜底：closeAll 可能在 connect 期间被调用（用户快速退出）；迟到的连接立即断开，
        // 否则 Room 会一直保持连接/重连（表现为「退出后还在重试」）。
        if (this.disposed) {
          try { room.disconnect() } catch {}
          console.log('[RtcEngine] 连接期间已退出，断开刚建立的 LiveKit 连接')
          return
        }
        const videoTrack = this.localStream.getVideoTracks()[0] || null
        const audioTrack = this.localStream.getAudioTracks()[0] || null
        if (videoTrack) this.publishedVideo = await this._publish('camera', 'video', videoTrack)
        if (audioTrack) this.publishedAudio = await this._publish('microphone', 'audio', audioTrack)
      } catch (e) {
        console.error('[RtcEngine] LiveKit 连接失败:', e instanceof Error ? e.message : String(e))
        try { room.disconnect() } catch {}
      }
    }

    // 发布或同名替换一条本地轨道（先 unpublish 同名旧轨再发，杜绝重复）
    async _publish(name, kind, track) {
      const C = LK()
      const lp = this.room && this.room.localParticipant
      if (!lp) return null
      for (const pub of lp.trackPublications.values()) {
        if (pub.trackName === name) {
          const old = pub.track
          if (old) await lp.unpublishTrack(old)
          try { old && old.stop() } catch {}
        }
      }
      if (!track) return null
      const lkTrack = kind === 'video'
        ? new C.LocalVideoTrack(track, {}, true)
        : new C.LocalAudioTrack(track, undefined, true)
      await lp.publishTrack(lkTrack, { name })
      return lkTrack
    }

    // ─── 兼容旧 API（LiveKit 自动订阅/建连，空操作保留签名） ───
    addPeer() { /* LiveKit 自动订阅 */ }
    handleSignal() { /* 无 P2P 信令 */ }
    closePeer() { /* LiveKit 由房间事件驱动 */ }

    // 断开 LiveKit 连接（必须调用，否则 autoReconnect 会一直重连）
    closeAll() {
      this.disposed = true
      try { this.room && this.room.disconnect() } catch {}
      this.room = null
      this.remoteTracksByUser.clear()
      this.publishedVideo = null
      this.publishedAudio = null
    }

    // 替换本地轨道（切换摄像头/麦克风/屏幕共享/开关摄像头）；track=null 取消发布。
    // 注意：room.js 自行管理 localStream（重建后传入新轨），这里只更新 LiveKit 上已发布的轨道。
    replaceTrack(track, kind) {
      const name = kind === 'video' ? 'camera' : 'microphone'
      void this._publish(name, kind, track).then((t) => {
        if (kind === 'video') this.publishedVideo = t
        else this.publishedAudio = t
      })
    }
  }
})()
