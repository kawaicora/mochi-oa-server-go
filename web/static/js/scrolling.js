/**
 * ScrollingSpectrum —— 瀑布滚动频谱（照抄客户端 src/renderer/src/media/ScrollingSpectrum.ts）
 * 深色背景 + 频率线 + 右侧钢琴琴键（C1~C10 对数刻度）。canvas.captureStream 作为视频轨。
 */
;(function () {
  const midiToHz = (() => {
    const cache = new Map()
    return function (midiNote) {
      if (cache.has(midiNote)) return cache.get(midiNote)
      const val = 440 * Math.pow(2, (midiNote - 69) / 12)
      cache.set(midiNote, val)
      return val
    }
  })()
  const MIDI_C1 = 24
  const MIDI_C10 = 108
  const FREQ_C1 = midiToHz(MIDI_C1)
  const FREQ_C10 = midiToHz(MIDI_C10)
  function getMidiRangeKeys(startMidi, endMidi) {
    const white = [], black = []
    for (let m = startMidi; m <= endMidi; m++) {
      const mod = m % 12
      if ([0, 2, 4, 5, 7, 9, 11].indexOf(mod) >= 0) white.push(m)
      else black.push(m)
    }
    return { white, black }
  }
  const KEYS = getMidiRangeKeys(MIDI_C1, MIDI_C10)
  const WHITE_KEYS = KEYS.white
  const BLACK_KEYS = KEYS.black

  window.ScrollingSpectrum = class ScrollingSpectrum {
    constructor() {
      this.running = false
      this.intervalId = null
      this.audioCtx = null
      this.source = null
      this.analyser = null
      this.stream = null
      this.canvas = null
      this.ctx = null
      this.offscreenCanvas = null
      this.offCtx = null
      this.dpr = 1
      this.data = new Uint8Array(0)
      this.sampleRate = 48000
      this.points = []
      this.logC1 = Math.log(FREQ_C1)
      this.logC10 = Math.log(FREQ_C10)
      this.colorCache = new Map()
    }

    freqToPy(hz, cssH) {
      const clampedHz = Math.max(FREQ_C1, Math.min(FREQ_C10, hz))
      const logF = Math.log(clampedHz)
      return cssH * (this.logC10 - logF) / (this.logC10 - this.logC1)
    }

    dbColor(t) {
      const c = Math.max(0, Math.min(1, t))
      const key = Math.round(c * 200)
      const cached = this.colorCache.get(key)
      if (cached) return cached
      const r = 255 * Math.pow(c, 0.8)
      const g = 255 * Math.pow(Math.max(0, c - 0.35) / 0.65, 2)
      const b = 0
      const lum = 0.2 + 0.8 * Math.pow(c, 0.7)
      const str = 'rgb(' + Math.round(r * lum) + ',' + Math.round(g * lum) + ',' + Math.round(b * lum) + ')'
      this.colorCache.set(key, str)
      return str
    }

    GetScrollingSpectrumStream(audioStream, opts) {
      opts = opts || {}
      const audioTrack = audioStream && audioStream.getAudioTracks()[0]
      if (!audioTrack) return null
      const width = opts.width || 1280
      const height = opts.height || 720
      const fftSize = opts.fftSize || 8192
      const scrollSpeed = opts.scrollSpeed || 6
      const minDb = opts.minDb || -80
      const maxDb = opts.maxDb || 0
      const multiplier = opts.multiplier || 2
      const background = opts.background || '#0b0f1a'
      const fps = opts.fps || 30
      const pianoWidthRatio = opts.pianoWidthRatio || 0.08
      this.dpr = opts.devicePixelRatio || window.devicePixelRatio || 1

      this.canvas = document.createElement('canvas')
      this.canvas.width = width * this.dpr
      this.canvas.height = height * this.dpr
      this.canvas.style.width = width + 'px'
      this.canvas.style.height = height + 'px'
      this.ctx = this.canvas.getContext('2d')
      this.ctx.setTransform(this.dpr, 0, 0, this.dpr, 0, 0)
      const waterfallCssW = width * (1 - pianoWidthRatio)
      this.offscreenCanvas = document.createElement('canvas')
      this.offscreenCanvas.width = waterfallCssW * this.dpr
      this.offscreenCanvas.height = height * this.dpr
      this.offCtx = this.offscreenCanvas.getContext('2d')
      this.offCtx.setTransform(this.dpr, 0, 0, this.dpr, 0, 0)
      try {
        this.audioCtx = new (window.AudioContext || window.webkitAudioContext)()
        this.source = this.audioCtx.createMediaStreamSource(new MediaStream([audioTrack]))
        this.analyser = this.audioCtx.createAnalyser()
        this.analyser.fftSize = fftSize
        this.analyser.smoothingTimeConstant = 0
        this.analyser.minDecibels = minDb
        this.analyser.maxDecibels = maxDb
        this.source.connect(this.analyser)
        this.sampleRate = this.audioCtx.sampleRate
        this.data = new Uint8Array(this.analyser.frequencyBinCount)
        if (this.audioCtx.state === 'suspended') void this.audioCtx.resume()
      } catch (e) { console.error('Audio init error:', e); return null }

      const binCount = fftSize / 2
      this.points.length = 0
      for (let i = 0; i < binCount; i++) this.points.push({ py: 0, v: 0 })

      const cssW = width
      const cssH = height
      const pianoW = cssW * pianoWidthRatio
      const pianoX = cssW - pianoW
      const waterfallW = cssW - pianoW

      const self = this
      const drawFrame = () => {
        if (!self.running || !self.canvas || !self.ctx || !self.offscreenCanvas || !self.offCtx) return
        if (self.analyser) {
          if (self.data.length !== self.analyser.frequencyBinCount) self.data = new Uint8Array(self.analyser.frequencyBinCount)
          try { self.analyser.getByteFrequencyData(self.data) } catch (err) { return }
        }
        // 离屏画布：旧画面左移
        self.offCtx.drawImage(self.offscreenCanvas, scrollSpeed, 0, waterfallW - scrollSpeed, cssH, 0, 0, waterfallW - scrollSpeed, cssH)
        self.offCtx.fillStyle = background
        self.offCtx.fillRect(waterfallW - scrollSpeed, 0, scrollSpeed, cssH)
        const halfW = scrollSpeed / 2
        const newX = waterfallW - halfW
        for (let bin = 0; bin < binCount; bin++) {
          const v = self.data[bin] / 255 * multiplier
          const binFreq = (bin / binCount) * (self.sampleRate / 2)
          const py = self.freqToPy(binFreq, cssH)
          const p = self.points[bin]
          p.py = py
          p.v = v
        }
        let currentColor = null
        for (let i = 0; i < self.points.length - 1; i++) {
          const p0 = self.points[i]
          const p1 = self.points[i + 1]
          const y0 = p0.py, y1 = p1.py
          const yMin = Math.min(y0, y1), yMax = Math.max(y0, y1)
          const len = yMax - yMin
          if (len < 0.01) continue
          const avgV = (p0.v + p1.v) / 2
          const color = self.dbColor(avgV)
          if (color !== currentColor) {
            if (currentColor !== null) { self.offCtx.closePath(); self.offCtx.fill() }
            self.offCtx.beginPath()
            self.offCtx.fillStyle = color
            currentColor = color
          }
          self.offCtx.moveTo(newX - halfW, y0)
          self.offCtx.lineTo(newX + halfW, y0)
          self.offCtx.lineTo(newX + halfW, y1)
          self.offCtx.lineTo(newX - halfW, y1)
        }
        if (currentColor !== null) self.offCtx.fill()

        self.ctx.drawImage(self.offscreenCanvas, 0, 0)
        self.ctx.fillStyle = background
        self.ctx.fillRect(pianoX, 0, pianoW, cssH)
        // 钢琴白键
        self.ctx.fillStyle = '#f5f5f5'
        for (let k = 0; k < WHITE_KEYS.length; k++) {
          const midi = WHITE_KEYS[k]
          const hzLow = midiToHz(midi - 0.5)
          const hzHigh = midiToHz(midi + 0.5)
          const pyLow = self.freqToPy(hzLow, cssH)
          const pyHigh = self.freqToPy(hzHigh, cssH)
          const top = Math.min(pyLow, pyHigh)
          const hh = Math.max(0, Math.abs(pyHigh - pyLow))
          if (hh < 0.1) continue
          self.ctx.fillRect(pianoX, top, pianoW, hh)
        }
        // 钢琴黑键
        self.ctx.fillStyle = '#1a1a1a'
        for (let k = 0; k < BLACK_KEYS.length; k++) {
          const midi = BLACK_KEYS[k]
          const hzLow = midiToHz(midi - 0.5)
          const hzHigh = midiToHz(midi + 0.5)
          const pyLow = self.freqToPy(hzLow, cssH)
          const pyHigh = self.freqToPy(hzHigh, cssH)
          const top = Math.min(pyLow, pyHigh)
          const hh = Math.max(0, Math.abs(pyHigh - pyLow))
          if (hh < 0.1) continue
          self.ctx.fillRect(pianoX + pianoW * 0.3, top, pianoW * 0.55, hh)
        }
        // Cx 八度标签
        self.ctx.fillStyle = '#ff00FF'
        self.ctx.textBaseline = 'middle'
        self.ctx.font = '8px sans-serif'
        const octaves = [24, 36, 48, 60, 72, 84, 96, 108]
        for (let o = 0; o < octaves.length; o++) {
          const midi = octaves[o]
          const hzMid = midiToHz(midi)
          const pyMid = self.freqToPy(hzMid, cssH)
          const oct = (midi - 24) / 12 + 1
          self.ctx.fillText('C' + oct, pianoX + pianoW * 0.72, pyMid)
        }
      }

      this.running = true
      this.intervalId = setInterval(() => { if (self.running) drawFrame() }, 1000 / fps)
      try {
        this.stream = this.canvas.captureStream(fps)
      } catch (e) { console.error('captureStream error:', e); this.stop(); return null }
      const videoTrack = this.stream.getVideoTracks()[0]
      videoTrack.addEventListener('ended', () => this.stop())
      return this.stream
    }

    stop() {
      this.running = false
      if (this.intervalId !== null) { clearInterval(this.intervalId); this.intervalId = null }
      if (this.source) this.source.disconnect()
      if (this.analyser) this.analyser.disconnect()
      if (this.audioCtx) void this.audioCtx.close()
      if (this.stream) { this.stream.getTracks().forEach((t) => t.stop()); this.stream = null }
      this.audioCtx = null; this.source = null; this.analyser = null
      this.canvas = null; this.ctx = null; this.offscreenCanvas = null; this.offCtx = null
      this.points.length = 0
      this.colorCache.clear()
    }
  }
})()
