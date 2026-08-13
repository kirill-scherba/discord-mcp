package uk.bmat.govorilka

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.util.Log
import androidx.appcompat.app.AppCompatActivity
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat

class MainActivity : AppCompatActivity() {

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        // Fresh activity = fresh start: allow the service to (re)start even
        // if the process survived a previous stopEverything().
        serviceStarted = false

        // IMPORTANT: no WebView. The web page (govorilka.bmat.uk) is a full
        // WebRTC client itself — loading it here would start a SECOND
        // PeerConnection, a second microphone capture and a second audio
        // output, fighting the native VoiceService over mic/audio routing.
        // All audio goes through the native service only.
        val root = android.widget.FrameLayout(this)

        val title = android.widget.TextView(this)
        title.text = "Говорилка (нативный режим)"
        title.textSize = 20f
        title.gravity = android.view.Gravity.CENTER
        root.addView(title, android.widget.FrameLayout.LayoutParams(
            android.view.ViewGroup.LayoutParams.MATCH_PARENT,
            android.view.ViewGroup.LayoutParams.WRAP_CONTENT
        ).apply { gravity = android.view.Gravity.CENTER })

        val stopBtn = android.widget.Button(this)
        stopBtn.text = "Стоп"
        val lp = android.widget.FrameLayout.LayoutParams(
            android.widget.FrameLayout.LayoutParams.WRAP_CONTENT,
            android.widget.FrameLayout.LayoutParams.WRAP_CONTENT
        )
        lp.gravity = android.view.Gravity.BOTTOM or android.view.Gravity.END
        lp.setMargins(0, 0, 24, 24)
        stopBtn.layoutParams = lp
        stopBtn.setOnClickListener { stopEverything() }
        root.addView(stopBtn)

        setContentView(root)

        // Микрофон нужен для нативного WebRTC (VoiceService).
        // FOREGROUND_SERVICE_MICROPHONE — normal-разрешение (в манифесте),
        // его НЕ запрашивают runtime. Запрашиваем только RECORD_AUDIO,
        // иначе requestPermissions с несуществующим разрешением виснет и
        // onRequestPermissionsResult не приходит → сервис не стартует.
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO)
            != PackageManager.PERMISSION_GRANTED
        ) {
            ActivityCompat.requestPermissions(this, arrayOf(Manifest.permission.RECORD_AUDIO), 1)
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
        // Reset the guard: the process may survive finishAndRemoveTask(),
        // and serviceStarted is a static that lives as long as the process.
        // Without the reset, the next launch would skip startVoiceService()
        // and the app would do nothing.
        serviceStarted = false
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

    override fun onBackPressed() {
        super.onBackPressed()
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
        Log.d("Govorilka", "onResume: приложение снова активно")
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
    }
}
