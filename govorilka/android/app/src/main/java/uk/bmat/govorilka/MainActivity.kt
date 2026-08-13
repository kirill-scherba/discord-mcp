package uk.bmat.govorilka

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.util.Log
import android.webkit.PermissionRequest
import android.webkit.WebChromeClient
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.appcompat.app.AppCompatActivity
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat

class MainActivity : AppCompatActivity() {

    private lateinit var webView: WebView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        // WebView + плавающая кнопка «Стоп» поверх.
        val root = android.widget.FrameLayout(this)
        webView = WebView(this)
        root.addView(webView, android.widget.FrameLayout.LayoutParams(
            android.view.ViewGroup.LayoutParams.MATCH_PARENT,
            android.view.ViewGroup.LayoutParams.MATCH_PARENT
        ))

        val stopBtn = android.widget.Button(this)
        stopBtn.text = "Стоп"
        val lp = android.widget.FrameLayout.LayoutParams(
            android.view.ViewGroup.LayoutParams.WRAP_CONTENT,
            android.view.ViewGroup.LayoutParams.WRAP_CONTENT
        )
        lp.gravity = android.view.Gravity.BOTTOM or android.view.Gravity.END
        lp.setMargins(0, 0, 24, 24)
        stopBtn.layoutParams = lp
        stopBtn.setOnClickListener { stopEverything() }
        root.addView(stopBtn)

        setContentView(root)

        configureWebView()

        // Микрофон нужен до загрузки страницы (WebRTC).
        // На Android 14+ нужно runtime-разрешение FOREGROUND_SERVICE_MICROPHONE,
        // иначе startForegroundService с типом microphone не запустится.
        val perms = mutableListOf(Manifest.permission.RECORD_AUDIO)
        if (Build.VERSION.SDK_INT >= 34) {
            perms.add("android.permission.FOREGROUND_SERVICE_MICROPHONE")
        }
        val missing = perms.filter {
            ContextCompat.checkSelfPermission(this, it) != PackageManager.PERMISSION_GRANTED
        }
        if (missing.isNotEmpty()) {
            ActivityCompat.requestPermissions(this, missing.toTypedArray(), 1)
        } else {
            // VoiceService: foreground + WakeLock — держит приложение в фоне
            // (карманный режим), как это делает Discord.
            startVoiceService()
        }
    }

    // Полная остановка: стоп сервиса и закрытие приложения.
    private fun stopEverything() {
        Log.d("Govorilka", "Стоп: останавливаю сервис и приложение")
        stopService(Intent(this, VoiceService::class.java))
        finishAndRemoveTask()
    }

    override fun onRequestPermissionsResult(
        requestCode: Int, permissions: Array<out String>, grantResults: IntArray
    ) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (grantResults.isNotEmpty() && grantResults[0] == PackageManager.PERMISSION_GRANTED) {
            startVoiceService()
        }
    }

    private fun configureWebView() {
        val settings = webView.settings
        settings.javaScriptEnabled = true
        settings.mediaPlaybackRequiresUserGesture = false
        settings.domStorageEnabled = true
        settings.setSupportZoom(false)

        // Разрешить getUserMedia (микрофон) внутри WebView.
        webView.webChromeClient = object : WebChromeClient() {
            override fun onPermissionRequest(request: PermissionRequest) {
                // WebRTC: grant mic (camera не нужна).
                request.grant(request.resources)
            }
        }
        webView.webViewClient = WebViewClient()

        // URL говорилки — https (микрофон работает только в secure context).
        webView.loadUrl(GOVORILKA_URL)
    }

    override fun onBackPressed() {
        if (webView.canGoBack()) webView.goBack() else super.onBackPressed()
    }

    // --- Diagnostics: lifecycle logging to see what happens in background ---
    override fun onPause() {
        super.onPause()
        Log.d("Govorilka", "onPause: приложение уходит в фон")
    }

    override fun onStop() {
        super.onStop()
        Log.d("Govorilka", "onStop: приложение остановлено")
    }

    override fun onResume() {
        super.onResume()
        Log.d("Govorilka", "onResume: приложение снова активно, url=${webView.url}")
    }

    override fun onDestroy() {
        Log.d("Govorilka", "onDestroy: приложение уничтожается")
        super.onDestroy()
    }

    private fun startVoiceService() {
        // Запускаем только один раз — иначе повторный startForegroundService
        // может упасть (ServiceNotFoundException / duplicate).
        if (serviceStarted) return
        serviceStarted = true
        val intent = Intent(this, VoiceService::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(intent)
        } else {
            startService(intent)
        }
    }

    companion object {
        private var serviceStarted = false
        // Временный URL — пока WebRTC-сервер на нашем домене.
        private const val GOVORILKA_URL = "https://govorilka.bmat.uk"
    }
}
