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

    // Native playback of the server's reply is handled by WebRTC itself
    // (WebRtcAudioTrack -> Java AudioTrack), which works in the background
    // thanks to the foreground service. No custom AudioTrack sink needed.

    override fun onCreate() {
        super.onCreate()
        Log.d("Govorilka", "VoiceService onCreate")
        acquireWakeLock()
        // Voice-communication audio mode: routes the WebRTC output track to
        // the speaker when the screen is off / app is backgrounded. Without
        // MODE_IN_COMMUNICATION, the USAGE_VOICE_COMMUNICATION AudioTrack is
        // created but stays silent (audio mode stays MODE_NORMAL).
        val am = getSystemService(Context.AUDIO_SERVICE) as android.media.AudioManager
        am.mode = android.media.AudioManager.MODE_IN_COMMUNICATION
        // Route the voice-communication stream to the loudspeaker (rear
        // speaker) instead of the earpiece (the one you put to your ear).
        am.isSpeakerphoneOn = true
        Log.d("Govorilka", "audio mode: MODE_IN_COMMUNICATION, speakerphone: ${am.isSpeakerphoneOn}")
        handlerThread = HandlerThread("govorilka-webrtc").apply { start() }
        handler = Handler(handlerThread!!.looper)
        initializeWebRTC()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        Log.d("Govorilka", "VoiceService onStartCommand")
        startForegroundCompat()
        connect()
        return START_STICKY
    }

    override fun onDestroy() {
        disconnect()
        // Restore normal audio mode so the device is not left in the
        // communication (call) state after the service stops.
        try {
            val am = getSystemService(Context.AUDIO_SERVICE) as android.media.AudioManager
            am.mode = android.media.AudioManager.MODE_NORMAL
        } catch (_: Exception) {}
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
        val audioConstraints = MediaConstraints()
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
            override fun onDataChannel(channel: org.webrtc.DataChannel?) {}
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
            }
        }
        val connection = factory!!.createPeerConnection(rtcConfig, observer) ?: return null
        connection.addTrack(audioTrack!!, listOf("govorilka-stream"))
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
                val request = Request.Builder()
                    .url("wss://govorilka.bmat.uk/signal")
                    .build()
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
                    }

                    override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                        Log.d("Govorilka", "WS closing: $code $reason")
                    }

                    override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                        Log.d("Govorilka", "WS failure: ${t.message}")
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

    private fun disconnect() {
        ws?.close(1000, "bye")
        ws = null
        wsHttpClient?.dispatcher?.executorService?.shutdown()
        wsHttpClient = null
        pc?.close()
        pc = null
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
}
