package uk.bmat.govorilka

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.Handler
import android.os.HandlerThread
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import androidx.core.app.NotificationCompat
import org.json.JSONObject
import org.webrtc.AudioSource
import org.webrtc.AudioTrack
import org.webrtc.DefaultVideoDecoderFactory
import org.webrtc.DefaultVideoEncoderFactory
import org.webrtc.EglBase
import org.webrtc.IceCandidate
import org.webrtc.MediaConstraints
import org.webrtc.MediaStream
import org.webrtc.PeerConnection
import org.webrtc.PeerConnectionFactory
import org.webrtc.RTCStatsReport
import org.webrtc.RtpSender
import org.webrtc.SessionDescription
import org.webrtc.VideoDecoderFactory
import org.webrtc.VideoEncoderFactory
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.ByteString

/**
 * Foreground service with NATIVE WebRTC (not WebView) — keeps the mic alive
 * with the screen off / app in background (pocket mode), like Discord.
 *
 * The WebView page in MainActivity is still shown as UI, but audio goes
 * through this native PeerConnection so it survives backgrounding.
 */
class VoiceService : Service() {

    private var wakeLock: PowerManager.WakeLock? = null
    private var handlerThread: HandlerThread? = null
    private var handler: Handler? = null
    private var ws: WebSocket? = null
    private var wsHttpClient: OkHttpClient? = null

    // WebRTC
    private var factory: PeerConnectionFactory? = null
    private var pc: PeerConnection? = null
    private var audioSource: AudioSource? = null
    private var audioTrack: AudioTrack? = null
    private var localStream: MediaStream? = null
    private var eglBase: EglBase? = null
    private var dc: org.webrtc.DataChannel? = null
    private var serverDc: org.webrtc.DataChannel? = null
    private var micSender: RtpSender? = null

    // Native playback of the server's reply is handled by WebRTC itself
    // (WebRtcAudioTrack -> Java AudioTrack), which works in the background
    // thanks to the foreground service. No custom AudioTrack sink needed.

    override fun onCreate() {
        super.onCreate()
        Log.d("Govorilka", "VoiceService onCreate")
        acquireWakeLock()
        // Apply the audio route chosen in MainActivity (auto/phone/speaker/bt).
        applyAudioRoute()
        handlerThread = HandlerThread("govorilka-webrtc").apply { start() }
        handler = Handler(handlerThread!!.looper)
        initializeWebRTC()
    }

    // Audio route selector — like the speaker button in call apps.
    //   "auto"   — follow the connected headset (default; BT SCO when a
    //              headset is connected, otherwise the phone speaker)
    //   "phone"  — earpiece + phone mic, BT disabled
    //   "speaker"— loudspeaker + phone mic (громкая связь)
    //   "bt"     — Bluetooth SCO headset (mic + audio via BT)
    private fun applyAudioRoute() {
        try {
            val am = getSystemService(Context.AUDIO_SERVICE) as android.media.AudioManager
            val prefs = getSharedPreferences("govorilka_prefs", Context.MODE_PRIVATE)
            val route = prefs.getString("route", "auto") ?: "auto"
            am.mode = android.media.AudioManager.MODE_IN_COMMUNICATION

            when (route) {
                "phone" -> {
                    am.isBluetoothScoOn = false
                    am.stopBluetoothSco()
                    am.isSpeakerphoneOn = false
                    Log.d("Govorilka", "audio route: phone (earpiece)")
                }
                "speaker" -> {
                    am.isBluetoothScoOn = false
                    am.stopBluetoothSco()
                    am.isSpeakerphoneOn = true
                    Log.d("Govorilka", "audio route: speaker (громкая связь)")
                }
                "bt" -> {
                    am.isSpeakerphoneOn = false
                    am.isBluetoothScoOn = false
                    am.startBluetoothSco()
                    am.isBluetoothScoOn = true
                    Log.d("Govorilka", "audio route: bluetooth SCO")
                }
                else -> { // auto: follow the headset (old behaviour)
                    am.isSpeakerphoneOn = false
                    am.isBluetoothScoOn = false
                    am.startBluetoothSco()
                    am.isBluetoothScoOn = true
                    Log.d("Govorilka", "audio route: auto (BT if connected)")
                }
            }
        } catch (e: Exception) {
            Log.d("Govorilka", "audio route error: ${e.message}")
        }
    }

    private fun disableBluetoothSco() {
        try {
            val am = getSystemService(Context.AUDIO_SERVICE) as android.media.AudioManager
            am.isBluetoothScoOn = false
            am.stopBluetoothSco()
            am.mode = android.media.AudioManager.MODE_NORMAL
            Log.d("Govorilka", "BT SCO disabled, mode NORMAL")
        } catch (e: Exception) {
            Log.d("Govorilka", "BT SCO disable error: ${e.message}")
        }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        Log.d("Govorilka", "VoiceService onStartCommand")
        startForegroundCompat()
        connect()
        startWatchdog()
        return START_STICKY
    }

    override fun onDestroy() {
        disconnect()
        disableBluetoothSco()
        stopLocalVosk()
        // Release WebRTC resources. Order matters: the MediaStream must be
        // disposed BEFORE its tracks — MediaStream.dispose() removes its
        // tracks, and a track that was already disposed causes an
        // IllegalStateException ("MediaStreamTrack has been disposed").
        localStream?.dispose()
        localStream = null
        audioTrack?.dispose()
        audioTrack = null
        audioSource?.dispose()
        audioSource = null
        handlerThread?.quitSafely()
        wakeLock?.let { if (it.isHeld) it.release() }
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    // --- WebRTC init ---

    private fun initializeWebRTC() {
        PeerConnectionFactory.initialize(
            PeerConnectionFactory.InitializationOptions.builder(this)
                .setEnableInternalTracer(false)
                .createInitializationOptions()
        )
        eglBase = EglBase.create()

        val encoderFactory: VideoEncoderFactory = DefaultVideoEncoderFactory(eglBase!!.eglBaseContext, true, true)
        val decoderFactory: VideoDecoderFactory = DefaultVideoDecoderFactory(eglBase!!.eglBaseContext)
        factory = PeerConnectionFactory.builder()
            .setVideoEncoderFactory(encoderFactory)
            .setVideoDecoderFactory(decoderFactory)
            .createPeerConnectionFactory()

        // Audio: capture mic into an AudioTrack (native, works in background).
        // DTX (discontinuous transmission): when there is silence, Opus does
        // not send audio frames (only rare comfort-noise). Saves battery and
        // network while the peer is "asleep" (server is not listening anyway).
        val audioConstraints = MediaConstraints()
        audioConstraints.mandatory.add(
            org.webrtc.MediaConstraints.KeyValuePair("googDtx", "true")
        )
        audioSource = factory!!.createAudioSource(audioConstraints)
        audioTrack = factory!!.createAudioTrack("govorilka-audio", audioSource)
        localStream = factory!!.createLocalMediaStream("govorilka-stream")
        localStream!!.addTrack(audioTrack!!)
    }

    private fun createPeerConnection(): PeerConnection? {
        val rtcConfig = PeerConnection.RTCConfiguration(
            listOf(PeerConnection.IceServer("stun:stun.l.google.com:19302"))
        )
        val observer = object : PeerConnection.Observer {
            override fun onSignalingChange(state: PeerConnection.SignalingState?) {}
            override fun onIceConnectionChange(state: PeerConnection.IceConnectionState?) {
                Log.d("Govorilka", "ICE state: $state")
            }
            override fun onIceConnectionReceivingChange(receiving: Boolean) {}
            override fun onIceGatheringChange(state: PeerConnection.IceGatheringState?) {}
            override fun onIceCandidate(candidate: IceCandidate) {
                sendSignal(JSONObject().apply {
                    put("signal", "candidate")
                    put("data", JSONObject().apply {
                        put("candidate", candidate.sdp)
                        put("sdpMid", candidate.sdpMid)
                        put("sdpMLineIndex", candidate.sdpMLineIndex)
                    })
                }.toString())
            }
            override fun onIceCandidatesRemoved(candidates: Array<out IceCandidate>?) {}
            override fun onAddStream(stream: MediaStream?) {}
            override fun onRemoveStream(stream: MediaStream?) {}
            override fun onDataChannel(channel: org.webrtc.DataChannel?) {
                Log.d("Govorilka", "DataChannel open: ${channel?.label()}")
                // Keep the server-created control channel: it is the one the
                // server listens on, so wake commands must go through it.
                if (channel?.label() == "govorilka-ctl") {
                    serverDc = channel
                }
                channel?.registerObserver(object : org.webrtc.DataChannel.Observer {
                    override fun onBufferedAmountChange(previousAmount: Long) {}
                    override fun onStateChange() {
                        Log.d("Govorilka", "DataChannel state: ${channel.state()}")
                    }
                    override fun onMessage(buffer: org.webrtc.DataChannel.Buffer) {
                        val data = buffer.data
                        val bytes = ByteArray(data.remaining())
                        data.get(bytes)
                        val msg = String(bytes, Charsets.UTF_8)
                        Log.d("Govorilka", "DataChannel msg: $msg")
                        handleControlMessage(msg)
                    }
                })
            }
            override fun onRenegotiationNeeded() {}
            override fun onAddTrack(receiver: org.webrtc.RtpReceiver?, streams: Array<out MediaStream>?) {}
            override fun onRemoveTrack(receiver: org.webrtc.RtpReceiver?) {}
            override fun onTrack(transceiver: org.webrtc.RtpTransceiver?) {
                // WebRTC plays the server's reply audio natively via its own
                // WebRtcAudioTrack -> Java AudioTrack. It survives background
                // periods thanks to the foreground service; no extra sink.
                val track = transceiver?.receiver?.track()
                if (track is AudioTrack) {
                    Log.d("Govorilka", "onTrack: remote audio track (WebRTC plays it)")
                }
            }
            override fun onConnectionChange(newState: PeerConnection.PeerConnectionState?) {
                Log.d("Govorilka", "PC state: $newState")
                if (newState == PeerConnection.PeerConnectionState.DISCONNECTED ||
                    newState == PeerConnection.PeerConnectionState.FAILED
                ) {
                    scheduleReconnect("pc $newState")
                } else if (newState == PeerConnection.PeerConnectionState.CONNECTED) {
                    reconnectAttempts = 0
                }
            }
        }
        val connection = factory!!.createPeerConnection(rtcConfig, observer) ?: return null
        micSender = connection.addTrack(audioTrack!!, listOf("govorilka-stream"))
        // Client -> server control channel (wake command; the server sends
        // sleep/wake state on ITS channel "govorilka-ctl"). Client channel
        // has a different label to avoid collision with the server one.
        val dcInit = org.webrtc.DataChannel.Init()
        dc = connection.createDataChannel("govorilka-ctl-client", dcInit)
        dc?.registerObserver(object : org.webrtc.DataChannel.Observer {
            override fun onBufferedAmountChange(previousAmount: Long) {}
            override fun onStateChange() {
                Log.d("Govorilka", "ctl DataChannel state: ${dc?.state()}")
            }
            override fun onMessage(buffer: org.webrtc.DataChannel.Buffer) {
                val data = buffer.data
                val bytes = ByteArray(data.remaining())
                data.get(bytes)
                handleControlMessage(String(bytes, Charsets.UTF_8))
            }
        })
        return connection
    }

    // --- Signaling (WebSocket) ---

    private fun connect() {
        handler?.post {
            disconnect()
            pc = createPeerConnection()
            try {
                val client = OkHttpClient.Builder()
                    .pingInterval(30, java.util.concurrent.TimeUnit.SECONDS)
                    .build()
                wsHttpClient = client
                // Basic auth credentials (same as the private gallery):
                // entered once in MainActivity, stored in SharedPreferences.
                val prefs = getSharedPreferences("govorilka_prefs", Context.MODE_PRIVATE)
                val login = prefs.getString("login", "") ?: ""
                val pass = prefs.getString("pass", "") ?: ""
                var reqBuilder = Request.Builder()
                    .url("wss://govorilka.bmat.uk/signal")
                if (login.isNotEmpty() && pass.isNotEmpty()) {
                    val cred = java.util.Base64.getEncoder()
                        .encodeToString("$login:$pass".toByteArray(Charsets.UTF_8))
                    reqBuilder = reqBuilder.header("Authorization", "Basic $cred")
                    Log.d("Govorilka", "WS auth: user=$login")
                }
                val request = reqBuilder.build()
                ws = client.newWebSocket(request, object : WebSocketListener() {
                    override fun onOpen(webSocket: WebSocket, response: Response) {
                        Log.d("Govorilka", "WS open, sending offer")
                        val pc = this@VoiceService.pc
                        if (pc != null) {
                            pc.createOffer(object : org.webrtc.SdpObserver {
                                override fun onCreateSuccess(desc: SessionDescription?) {
                                    Log.d("Govorilka", "offer created, setLocalDescription")
                                    pc.setLocalDescription(object : org.webrtc.SdpObserver {
                                        override fun onCreateSuccess(desc: SessionDescription?) {}
                                        override fun onSetSuccess() {
                                            Log.d("Govorilka", "setLocalDescription ok, sending offer")
                                            sendSignal(JSONObject().apply {
                                                put("signal", "offer")
                                                put("data", JSONObject().apply {
                                                    put("type", desc?.type?.canonicalForm())
                                                    put("sdp", desc?.description)
                                                })
                                            }.toString())
                                        }
                                        override fun onCreateFailure(p0: String?) {
                                            Log.d("Govorilka", "setLocal create fail: $p0")
                                        }
                                        override fun onSetFailure(p0: String?) {
                                            Log.d("Govorilka", "setLocal set fail: $p0")
                                        }
                                    }, desc)
                                }
                                override fun onSetSuccess() {}
                                override fun onCreateFailure(p0: String?) {
                                    Log.d("Govorilka", "createOffer fail: $p0")
                                }
                                override fun onSetFailure(p0: String?) {
                                    Log.d("Govorilka", "createOffer set fail: $p0")
                                }
                            }, MediaConstraints())
                        }
                    }

                    override fun onMessage(webSocket: WebSocket, text: String) {
                        handler?.post { handleSignal(text) }
                    }

                    override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                        Log.d("Govorilka", "WS closed: $code $reason")
                        scheduleReconnect("ws closed")
                    }

                    override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                        Log.d("Govorilka", "WS closing: $code $reason")
                    }

                    override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                        Log.d("Govorilka", "WS failure: ${t.message}")
                        scheduleReconnect("ws failure: ${t.message}")
                    }
                })
            } catch (e: Exception) {
                Log.d("Govorilka", "WS connect error: ${e.message}")
            }
        }
    }

    private fun handleSignal(message: String?) {
        if (message == null) return
        try {
            val msg = JSONObject(message)
            val pc = this.pc ?: return
            when (msg.getString("signal")) {
                "answer" -> {
                    val data = msg.getJSONObject("data")
                    val sdp = SessionDescription(
                        SessionDescription.Type.ANSWER, data.getString("sdp")
                    )
                    pc.setRemoteDescription(object : org.webrtc.SdpObserver {
                        override fun onCreateSuccess(desc: SessionDescription?) {}
                        override fun onSetSuccess() { Log.d("Govorilka", "Connected!") }
                        override fun onCreateFailure(p0: String?) {}
                        override fun onSetFailure(p0: String?) {}
                    }, sdp)
                }
                "candidate" -> {
                    val data = msg.getJSONObject("data")
                    pc.addIceCandidate(IceCandidate(
                        data.getString("sdpMid"),
                        data.getInt("sdpMLineIndex"),
                        data.getString("candidate")
                    ))
                }
            }
        } catch (e: Exception) {
            Log.d("Govorilka", "signal parse error: ${e.message}")
        }
    }

    private fun sendSignal(s: String) {
        ws?.send(s)
    }

    // Handle control messages from the server over the DataChannel:
    //   {"state":"sleep"}  — server went to sleep, mute the mic track and
    //                        start the local Vosk wake-word listener
    //   {"state":"awake"}  — server woke up, unmute the mic track
    private fun handleControlMessage(msg: String) {
        try {
            val obj = org.json.JSONObject(msg)
            when (obj.optString("state")) {
                "sleep" -> {
                    Log.d("Govorilka", "server sleep -> mute mic + start local vosk")
                    handler?.postDelayed({
                        setMicMuted(true)
                        startLocalVosk()
                    }, 10000)
                }
                "awake" -> {
                    Log.d("Govorilka", "server awake -> unmute mic, stop local vosk")
                    handler?.post {
                        setMicMuted(false)
                        stopLocalVosk()
                    }
                }
            }
        } catch (e: Exception) {
            Log.d("Govorilka", "control msg error: ${e.message}")
        }
    }

    // Local Vosk wake-word detector: listens to the mic while the server
    // sleeps (mic track is muted -> zero network), and wakes the server
    // via a DataChannel command when "Барон" is heard.
    private var localVosk: WakeVoskDetector? = null

    private fun startLocalVosk() {
        if (localVosk?.isRunning == true) return
        val vosk = WakeVoskDetector(this)
        vosk.setOnWake {
            Log.d("Govorilka", "local vosk heard Барон -> wake server")
            setMicMuted(false)
            sendWake()
        }
        localVosk = vosk
        vosk.start()
    }

    private fun stopLocalVosk() {
        localVosk?.stop()
        localVosk = null
    }

    // Mute/unmute the outgoing mic track. When muted, WebRTC stops sending
    // audio — zero network/battery cost while the server is asleep.
    private fun setMicMuted(muted: Boolean) {
        val sender = micSender ?: return
        val track = sender.track()
        if (track != null && track.enabled() == muted) {
            track.setEnabled(!muted)
            Log.d("Govorilka", "mic track ${if (muted) "muted" else "unmuted"}")
        }
    }

    // Send a wake command to the server over the DataChannel (used by the
    // client-side Vosk when it hears "Барон" while the server sleeps).
    // The server listens on ITS channel "govorilka-ctl", so the command
    // must go through that channel — the client-created channel is not
    // accepted by the server and stays closed.
    private fun sendWake() {
        val ch = serverDc ?: dc ?: return
        if (ch.state() == org.webrtc.DataChannel.State.OPEN) {
            ch.send(org.webrtc.DataChannel.Buffer(
                java.nio.ByteBuffer.wrap("{\"cmd\":\"wake\"}".toByteArray()),
                false
            ))
            Log.d("Govorilka", "wake command sent to server")
        } else {
            Log.d("Govorilka", "wake NOT sent, dc state: ${ch.state()}")
        }
    }

    private fun disconnect() {
        ws?.close(1000, "bye")
        ws = null
        wsHttpClient?.dispatcher?.executorService?.shutdown()
        wsHttpClient = null
        pc?.close()
        pc = null
        // NOTE: audioSource/audioTrack are NOT disposed here — they are
        // created once in initializeWebRTC() and reused across reconnects
        // (connect() -> disconnect() runs on every reconnect; disposing here
        // would NPE on the next addTrack). They are released in onDestroy.
    }

    // --- Auto-reconnect ---
    // The service must survive network changes (Wi-Fi -> mobile, tunnel,
    // server restart). On any WS failure / PC disconnect we tear down and
    // re-connect with a small backoff. A watchdog re-checks the connection
    // periodically so a stale-but-open WS (e.g. server vanished) is caught.

    private var reconnectAttempts = 0
    private var reconnectScheduled = false
    private var lastConnectedAt = 0L

    private fun scheduleReconnect(reason: String) {
        if (reconnectScheduled) return
        reconnectScheduled = true
        reconnectAttempts++
        val delay = (3000L * reconnectAttempts).coerceAtMost(15000L)
        Log.d("Govorilka", "reconnect in ${delay}ms (attempt $reconnectAttempts, $reason)")
        handler?.postDelayed({
            reconnectScheduled = false
            if (pc?.connectionState() != PeerConnection.PeerConnectionState.CONNECTED) {
                Log.d("Govorilka", "reconnecting...")
                connect()
            } else {
                Log.d("Govorilka", "already connected, skip reconnect")
            }
        }, delay)
    }

    // Periodic watchdog: if the PC is not CONNECTED for a while (server died
    // without closing the WS, TCP half-open after network switch), force a
    // reconnect. Runs every 15s.
    private var watchdogRunning = false
    private fun startWatchdog() {
        if (watchdogRunning) return
        watchdogRunning = true
        handler?.postDelayed(object : Runnable {
            override fun run() {
                val state = pc?.connectionState()
                val connected = state == PeerConnection.PeerConnectionState.CONNECTED
                if (!connected) {
                    Log.d("Govorilka", "watchdog: pc=$state, reconnecting")
                    connect()
                }
                handler?.postDelayed(this, 15000)
            }
        }, 15000)
    }

    private fun acquireWakeLock() {
        val pm = getSystemService(Context.POWER_SERVICE) as PowerManager
        wakeLock = pm.newWakeLock(
            PowerManager.PARTIAL_WAKE_LOCK,
            "govorilka::pocket"
        ).apply {
            setReferenceCounted(false)
            acquire(24 * 60 * 60 * 1000L)
        }
    }

    private var isForeground = false

    private fun startForegroundCompat() {
        if (isForeground) return
        isForeground = true
        val channelId = "govorilka"
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                channelId, "Говорилка", NotificationManager.IMPORTANCE_LOW
            )
            getSystemService(NotificationManager::class.java).createNotificationChannel(channel)
        }

        val intent = Intent(this, MainActivity::class.java)
        val pi = PendingIntent.getActivity(
            this, 0, intent,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )

        val notification: Notification = NotificationCompat.Builder(this, channelId)
            .setSmallIcon(android.R.drawable.ic_btn_speak_now)
            .setContentTitle("Говорилка")
            .setContentText("Работает в кармане")
            .setContentIntent(pi)
            .setOngoing(true)
            .build()

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(1, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE)
        } else {
            startForeground(1, notification)
        }
    }

    companion object {
        @Volatile
        private var appContext: Context? = null

        // Called from MainActivity when the user switches the audio route:
        // applies the new route to the running service WITHOUT restarting
        // WebRTC — Android re-routes the AudioTrack output live.
        fun applyRouteImmediate(context: Context) {
            appContext = context.applicationContext
            val ctx = appContext ?: return
            try {
                val am = ctx.getSystemService(Context.AUDIO_SERVICE) as android.media.AudioManager
                val prefs = ctx.getSharedPreferences("govorilka_prefs", Context.MODE_PRIVATE)
                val route = prefs.getString("route", "auto") ?: "auto"
                am.mode = android.media.AudioManager.MODE_IN_COMMUNICATION
                when (route) {
                    "phone" -> {
                        am.isBluetoothScoOn = false
                        am.stopBluetoothSco()
                        am.isSpeakerphoneOn = false
                    }
                    "speaker" -> {
                        am.isBluetoothScoOn = false
                        am.stopBluetoothSco()
                        am.isSpeakerphoneOn = true
                    }
                    "bt" -> {
                        am.isSpeakerphoneOn = false
                        am.isBluetoothScoOn = false
                        am.startBluetoothSco()
                        am.isBluetoothScoOn = true
                    }
                    else -> {
                        am.isSpeakerphoneOn = false
                        am.isBluetoothScoOn = false
                        am.startBluetoothSco()
                        am.isBluetoothScoOn = true
                    }
                }
                Log.d("Govorilka", "audio route changed live: $route")
            } catch (e: Exception) {
                Log.d("Govorilka", "audio route live error: ${e.message}")
            }
        }
    }
}
