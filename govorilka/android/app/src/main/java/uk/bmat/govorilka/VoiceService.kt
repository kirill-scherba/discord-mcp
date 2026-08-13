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
import java.net.URI
import org.java_websocket.client.WebSocketClient
import org.java_websocket.handshake.ServerHandshake

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
    private var ws: WebSocketClient? = null

    // WebRTC
    private var factory: PeerConnectionFactory? = null
    private var pc: PeerConnection? = null
    private var audioSource: AudioSource? = null
    private var audioTrack: AudioTrack? = null
    private var localStream: MediaStream? = null
    private var eglBase: EglBase? = null

    override fun onCreate() {
        super.onCreate()
        acquireWakeLock()
        handlerThread = HandlerThread("govorilka-webrtc").apply { start() }
        handler = Handler(handlerThread!!.looper)
        initializeWebRTC()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        startForegroundCompat()
        connect()
        return START_STICKY
    }

    override fun onDestroy() {
        disconnect()
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
            override fun onTrack(transceiver: org.webrtc.RtpTransceiver?) {}
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
                ws = object : WebSocketClient(URI("wss://govorilka.bmat.uk/signal")) {
                    override fun onOpen(handshakedata: ServerHandshake?) {
                        Log.d("Govorilka", "WS open, sending offer")
                        val pc = this@VoiceService.pc
                        if (pc != null) {
                            pc.createOffer(object : org.webrtc.SdpObserver {
                                override fun onCreateSuccess(desc: SessionDescription?) {
                                    sendSignal(JSONObject().apply {
                                        put("signal", "offer")
                                        put("data", JSONObject().apply {
                                            put("type", desc?.type?.canonicalForm())
                                            put("sdp", desc?.description)
                                        })
                                    }.toString())
                                }
                                override fun onSetSuccess() {}
                                override fun onCreateFailure(p0: String?) {}
                                override fun onSetFailure(p0: String?) {}
                            }, MediaConstraints())
                        }
                    }

                    override fun onMessage(message: String?) {
                        handler?.post { handleSignal(message) }
                    }

                    override fun onClose(code: Int, reason: String?, remote: Boolean) {
                        Log.d("Govorilka", "WS closed: $code $reason")
                    }

                    override fun onError(ex: Exception?) {
                        Log.d("Govorilka", "WS error: ${ex?.message}")
                    }
                }
                ws!!.connect()
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
        ws?.close()
        ws = null
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
