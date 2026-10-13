/**
 * VoiceAvatarSpectrum —— 头像 + 实时环形频谱（照抄客户端 src/renderer/src/media/VoiceSpectrum.ts）
 * 圆形头像 + 左右声道各半圆环 FFT 频谱；canvas.captureStream 作为无摄像头时的视频轨。
 * 有头像画头像图，无头像画渐变圆（首字由调用方负责）。
 */
;(function () {
  window.VoiceAvatarSpectrum = class VoiceAvatarSpectrum {
    constructor(config) {
      this.config = Object.assign({
        fftSize: 2048, minHz: 20, maxHz: 2000, barCount: 180, multiplier: 2.0,
        minDb: -80, maxDb: 0, innerOffset: 5, outerOffset: 80, lineWidth: 8, fps: 60
      }, config || {})
    }

    GetVoiceAvatarStream(audioStream, opts) {
      opts = opts || {}
      const track = audioStream && audioStream.getAudioTracks()[0]
      if (!track) return null

      const h = opts.size || 1080
      const w = Math.round((h * 16) / 9)
      const dpr = window.devicePixelRatio || 1
      const canvas = document.createElement('canvas')
      canvas.width = w * dpr
      canvas.height = h * dpr
      const g = canvas.getContext('2d')
      let timerId = null
      let running = true
      let audioCtx = null
      let source = null, splitter = null, analyserL = null, analyserR = null

      const chunk = 4096
      const limitTopHz = 2000
      let dataL = new Uint8Array(0)
      let dataR = new Uint8Array(0)
      let avatarImg = null

      const cx = w / 2
      const cy = h / 2
      const faceR = Math.round(h * 0.36)
      const avatarGrad = g.createLinearGradient(cx - faceR, cy - faceR, cx + faceR, cy + faceR)
      avatarGrad.addColorStop(0, '#1677ff')
      avatarGrad.addColorStop(1, '#5cb6ff')
      const nickFirst = opts.nick ? String(opts.nick).charAt(0).toUpperCase() : null

      const alphaCache = new Map()
      const getColorL = (v) => {
        const a = 0.2 + v * 0.65
        const key = Math.round(a * 100)
        let c = alphaCache.get(key)
        if (!c) { c = 'rgba(56,132,255,' + a.toFixed(2) + ')'; alphaCache.set(key, c) }
        return c
      }
      const getColorR = (v) => {
        const a = 0.2 + v * 0.65
        const key = Math.round(a * 100)
        let c = alphaCache.get(key)
        if (!c) { c = 'rgba(0,200,180,' + a.toFixed(2) + ')'; alphaCache.set(key, c) }
        return c
      }

      try {
        audioCtx = new (window.AudioContext || window.webkitAudioContext)()
        source = audioCtx.createMediaStreamSource(new MediaStream([track]))
        splitter = audioCtx.createChannelSplitter(2)
        source.connect(splitter)
        analyserL = audioCtx.createAnalyser()
        analyserL.fftSize = chunk
        analyserL.smoothingTimeConstant = 0.0
        analyserL.minDecibels = this.config.minDb
        analyserL.maxDecibels = this.config.maxDb
        analyserR = audioCtx.createAnalyser()
        analyserR.fftSize = chunk
        analyserR.smoothingTimeConstant = 0.0
        analyserR.minDecibels = this.config.minDb
        analyserR.maxDecibels = this.config.maxDb
        splitter.connect(analyserL, 0)
        splitter.connect(analyserR, 1)
        if (audioCtx.state === 'suspended') void audioCtx.resume()
        dataL = new Uint8Array(analyserL.frequencyBinCount)
        dataR = new Uint8Array(analyserR.frequencyBinCount)
      } catch (e) { /* 音频分析不可用：仍绘制静态头像 */ }

      if (opts.avatar) {
        const img = new Image()
        img.crossOrigin = 'anonymous'
        img.onload = () => { avatarImg = img }
        img.onerror = () => { avatarImg = null }
        img.src = opts.avatar
      }

      const drawHalf = (data, startA, endA, getColor) => {
        const inner = faceR + this.config.innerOffset
        const outer = faceR + this.config.outerOffset
        const rate = (audioCtx && audioCtx.sampleRate) || 48000
        const n = Math.min(data.length, Math.max(1, Math.floor((limitTopHz / rate) * chunk)))
        g.lineWidth = this.config.lineWidth
        g.lineCap = 'round'
        for (let i = 0; i < n; i++) {
          const v = (data[i] / 255) * this.config.multiplier
          const len = inner + v * (outer - inner)
          const a = startA + (i / n) * (endA - startA)
          g.strokeStyle = getColor(v)
          g.beginPath()
          g.moveTo(cx + Math.cos(a) * inner, cy + Math.sin(a) * inner)
          g.lineTo(cx + Math.cos(a) * len, cy + Math.sin(a) * len)
          g.stroke()
        }
      }

      const drawFrame = () => {
        if (!running) return
        g.setTransform(dpr, 0, 0, dpr, 0, 0)
        g.clearRect(0, 0, w, h)
        if (avatarImg && avatarImg.complete && avatarImg.naturalWidth > 0) {
          g.save()
          g.beginPath()
          g.arc(cx, cy, faceR, 0, Math.PI * 2)
          g.clip()
          g.drawImage(avatarImg, cx - faceR, cy - faceR, faceR * 2, faceR * 2)
          g.restore()
        } else {
          g.beginPath()
          g.arc(cx, cy, faceR, 0, Math.PI * 2)
          g.fillStyle = avatarGrad
          g.fill()
          if (nickFirst) {
            g.fillStyle = 'rgba(255,255,255,0.92)'
            g.font = 'bold ' + Math.round(faceR * 1.05) + 'px -apple-system,"Segoe UI","Microsoft YaHei",sans-serif'
            g.textAlign = 'center'
            g.textBaseline = 'middle'
            g.fillText(nickFirst, cx, cy + Math.round(faceR * 0.02))
          }
        }
        if (analyserL && analyserR) {
          analyserL.getByteFrequencyData(dataL)
          analyserR.getByteFrequencyData(dataR)
          drawHalf(dataL, Math.PI / 2, (Math.PI * 3) / 2, getColorL)
          drawHalf(dataR, -Math.PI / 2, Math.PI / 2, getColorR)
        }
      }

      drawFrame()
      timerId = window.setInterval(drawFrame, 1000 / this.config.fps)
      let stream
      try {
        stream = canvas.captureStream(this.config.fps)
      } catch (e) {
        running = false
        if (timerId) clearInterval(timerId)
        if (audioCtx) void audioCtx.close()
        return null
      }
      const stop = () => {
        running = false
        if (timerId) { clearInterval(timerId); timerId = null }
        if (source) source.disconnect()
        if (splitter) splitter.disconnect()
        if (analyserL) analyserL.disconnect()
        if (analyserR) analyserR.disconnect()
        if (audioCtx) void audioCtx.close()
      }
      stream.__specStop = stop
      return stream
    }
  }
})()
