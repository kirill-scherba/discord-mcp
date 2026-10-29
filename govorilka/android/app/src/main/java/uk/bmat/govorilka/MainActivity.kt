package uk.bmat.govorilka

import android.Manifest
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.util.Log
import android.view.Gravity
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat

class MainActivity : AppCompatActivity() {

    private lateinit var loginInput: EditText
    private lateinit var passInput: EditText
    private var connectBtn: android.widget.Button? = null
    private var stopBtn: android.widget.Button? = null

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
        val root = FrameLayout(this)

        // Column: title, login/password fields, connect button.
        val column = LinearLayout(this)
        column.orientation = LinearLayout.VERTICAL
        column.setPadding(48, 48, 48, 48)
        root.addView(column, FrameLayout.LayoutParams(
            FrameLayout.LayoutParams.MATCH_PARENT,
            FrameLayout.LayoutParams.WRAP_CONTENT
        ).apply { gravity = Gravity.CENTER })

        val title = TextView(this)
        title.text = "Говорилка (нативный режим)"
        title.textSize = 22f
        title.gravity = Gravity.CENTER
        column.addView(title)

        loginInput = EditText(this)
        loginInput.hint = "Логин"
        loginInput.setText(savedLogin())
        column.addView(loginInput)

        passInput = EditText(this)
        passInput.hint = "Пароль"
        passInput.inputType = android.text.InputType.TYPE_CLASS_TEXT or
            android.text.InputType.TYPE_TEXT_VARIATION_PASSWORD
        passInput.setText(savedPass())
        column.addView(passInput)

        // Server selector: Hetzner (with server-side Vosk, for web) or RU
        // VPS (lightweight, no server Vosk — the app's local Vosk wakes it).
        val serverLabel = TextView(this)
        serverLabel.text = "Сервер:"
        serverLabel.textSize = 16f
        column.addView(serverLabel)

        val serverGroup = android.widget.RadioGroup(this)
        serverGroup.orientation = android.widget.RadioGroup.VERTICAL
        val servers = arrayOf(
            "hetzner" to "Hetzner (govorilka.bmat.uk)",
            "ru" to "RU VPS (govorilka-ru.bmat.uk)"
        )
        val savedServer = savedServer()
        var srvId = 2000
        for ((key, label) in servers) {
            val rb = android.widget.RadioButton(this)
            rb.id = srvId++
            rb.text = label
            rb.tag = key
            rb.isChecked = (key == savedServer)
            serverGroup.addView(rb)
        }
        serverGroup.setOnCheckedChangeListener { _, checkedId ->
            val rb = serverGroup.findViewById<android.widget.RadioButton>(checkedId)
            val key = rb?.tag as? String ?: "hetzner"
            getSharedPreferences(PREFS, Context.MODE_PRIVATE)
                .edit().putString("server", key).apply()
            Log.d("Govorilka", "server set: $key")
            // Apply to the running service immediately (reconnect with new host).
            VoiceService.applyServerImmediate(this)
        }
        column.addView(serverGroup)

        // Audio route selector (like the speaker button in call apps).
        val routeLabel = TextView(this)
        routeLabel.text = "Звук:"
        routeLabel.textSize = 16f
        column.addView(routeLabel)

        val routeGroup = android.widget.RadioGroup(this)
        routeGroup.orientation = android.widget.RadioGroup.VERTICAL
        val routes = arrayOf(
            "auto" to "Авто (наушники, если подключены)",
            "phone" to "Телефон (разговорный)",
            "speaker" to "Громкая связь",
            "bt" to "Блютус"
        )
        val savedRoute = savedRoute()
        var nextId = 1000
        for ((key, label) in routes) {
            val rb = android.widget.RadioButton(this)
            rb.id = nextId++
            rb.text = label
            rb.tag = key
            rb.isChecked = (key == savedRoute)
            routeGroup.addView(rb)
        }
        routeGroup.setOnCheckedChangeListener { _, checkedId ->
            val rb = routeGroup.findViewById<android.widget.RadioButton>(checkedId)
            val key = rb?.tag as? String ?: "auto"
            getSharedPreferences(PREFS, Context.MODE_PRIVATE)
                .edit().putString("route", key).apply()
            Log.d("Govorilka", "audio route set: $key")
            // Apply immediately to the running service (no restart needed).
            VoiceService.applyRouteImmediate(this)
        }
        column.addView(routeGroup)

        val connectBtn = android.widget.Button(this)
        connectBtn.text = "Подключиться"
        connectBtn.setOnClickListener { saveAndConnect() }
        column.addView(connectBtn)
        this.connectBtn = connectBtn

        val stopBtn = android.widget.Button(this)
        stopBtn.text = "Стоп"
        stopBtn.setOnClickListener { stopEverything() }
        column.addView(stopBtn)
        this.stopBtn = stopBtn

        setContentView(root)
        updateButtons()

        // Микрофон нужен для нативного WebRTC (VoiceService).
        // FOREGROUND_SERVICE_MICROPHONE — normal-разрешение (в манифесте),
        // его НЕ запрашивают runtime. Запрашиваем только RECORD_AUDIO,
        // иначе requestPermissions с несуществующим разрешением виснет и
        // onRequestPermissionsResult не приходит → сервис не стартует.
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO)
            != PackageManager.PERMISSION_GRANTED
        ) {
            ActivityCompat.requestPermissions(this, arrayOf(Manifest.permission.RECORD_AUDIO), 1)
        }
        // No auto-start: the service runs only after the user presses
        // "Подключиться" and stops with "Стоп" or the sleep timeout.
    }

    // Save credentials and start the service (pocket mode).
    private fun saveAndConnect() {
        val login = loginInput.text.toString().trim()
        val pass = passInput.text.toString()
        if (login.isEmpty() || pass.isEmpty()) {
            Log.d("Govorilka", "креды не заполнены")
            return
        }
        getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            .edit()
            .putString("login", login)
            .putString("pass", pass)
            .apply()
        Log.d("Govorilka", "креды сохранены, подключаюсь")
        startVoiceService()
    }

    private fun savedLogin(): String =
        getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString("login", "") ?: ""

    private fun savedPass(): String =
        getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString("pass", "") ?: ""

    private fun savedRoute(): String =
        getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString("route", "auto") ?: "auto"

    private fun savedServer(): String =
        getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString("server", "hetzner") ?: "hetzner"

    // Полная остановка: стоп сервиса и закрытие приложения.
    private fun stopEverything() {
        Log.d("Govorilka", "Стоп: останавливаю сервис и приложение")
        stopService(Intent(this, VoiceService::class.java))
        // Reset the guard: the process may survive finishAndRemoveTask(),
        // and serviceStarted is a static that lives as long as the process.
        // Without the reset, the next launch would skip startVoiceService()
        // and the app would do nothing.
        serviceStarted = false
        updateButtons()
        finishAndRemoveTask()
    }

    override fun onRequestPermissionsResult(
        requestCode: Int, permissions: Array<out String>, grantResults: IntArray
    ) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (grantResults.isNotEmpty() && grantResults[0] == PackageManager.PERMISSION_GRANTED) {
            Log.d("Govorilka", "permission granted")
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
        // Reflect the current service state on the buttons (it may have
        // stopped via the sleep timeout while the app was in the background).
        updateButtons()
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
        // Let the service onCreate run, then reflect its state on the buttons.
        android.os.Handler(android.os.Looper.getMainLooper()).postDelayed({
            updateButtons()
        }, 500)
    }

    // The buttons reflect the service state: "Стоп" is enabled while the
    // service runs (listening or asleep), "Подключиться" is enabled when
    // the service is off.
    private fun updateButtons() {
        val running = VoiceService.isRunning()
        connectBtn?.isEnabled = !running
        stopBtn?.isEnabled = running
        Log.d("Govorilka", "buttons updated: running=$running")
    }

    companion object {
        private const val PREFS = "govorilka_prefs"
        private var serviceStarted = false
    }
}
