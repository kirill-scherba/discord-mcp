package uk.bmat.govorilka

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
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

        webView = WebView(this)
        setContentView(webView)

        configureWebView()

        // Микрофон нужен до загрузки страницы (WebRTC).
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO)
            != PackageManager.PERMISSION_GRANTED
        ) {
            ActivityCompat.requestPermissions(this, arrayOf(Manifest.permission.RECORD_AUDIO), 1)
        }
        // NOTE: VoiceService (pocket mode) temporarily disabled to isolate
        // the crash. Will be re-enabled once the WebView part works.
    }

    override fun onRequestPermissionsResult(
        requestCode: Int, permissions: Array<out String>, grantResults: IntArray
    ) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        // VoiceService disabled for now; page loads regardless.
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
