package uk.bmat.govorilka

import android.content.Context
import android.util.Log
import org.vosk.Model
import org.vosk.Recognizer
import java.io.File
import java.io.FileOutputStream
import java.util.zip.ZipInputStream

/**
 * Client-side Vosk wake-word detector. Listens to the microphone locally
 * while the server is asleep, and reports when "Барон" is heard. This lets
 * the phone keep the mic track muted (zero network) and still wake the
 * server via a DataChannel command.
 *
 * The Vosk model is shipped as a zip in assets and extracted to the app's
 * files dir on first use.
 */
class WakeVoskDetector(private val context: Context) {

    private var model: Model? = null
    private var rec: Recognizer? = null
    private var thread: Thread? = null
    private var running = false
    private var onWake: (() -> Unit)? = null

    fun setOnWake(cb: () -> Unit) {
        onWake = cb
    }

    /** Extract the model from assets (once) and return its dir. */
    private fun ensureModel(): File {
        val dest = File(context.filesDir, "vosk-model-small-ru-0.22")
        // The zip contains a top-level folder "vosk-model-small-ru-0.22/",
        // so the actual model lives at dest/vosk-model-small-ru-0.22.
        val modelDir = File(dest, "vosk-model-small-ru-0.22")
        val ok = File(modelDir, "am/final.mdl").exists() &&
            File(modelDir, "graph/Gr.fst").exists()
        if (ok) {
            return modelDir
        }
        // Remove a possibly incomplete previous extraction.
        if (dest.exists()) {
            dest.deleteRecursively()
        }
        Log.d("Govorilka", "extracting vosk model...")
        val start = System.currentTimeMillis()
        val zip = ZipInputStream(context.assets.open("vosk-model-small-ru-0.22.zip"))
        var entry = zip.nextEntry
        val buf = ByteArray(65536)
        while (entry != null) {
            val file = File(dest, entry.name)
            if (entry.isDirectory) {
                file.mkdirs()
            } else {
                file.parentFile?.mkdirs()
                FileOutputStream(file).use { out ->
                    var n: Int
                    while (zip.read(buf).also { n = it } != -1) {
                        out.write(buf, 0, n)
                    }
                }
            }
            zip.closeEntry()
            entry = zip.nextEntry
        }
        zip.close()
        Log.d("Govorilka", "model extracted in ${(System.currentTimeMillis()-start)/1000}s")
        return modelDir
    }

    /** Start listening for the wake word on a background thread. */
    fun start() {
        if (running) return
        running = true
        thread = Thread {
            try {
                val modelDir = ensureModel()
                val m = Model(modelDir.absolutePath)
                val r = Recognizer(m, 48000f)
                model = m
                rec = r

                val am = context.getSystemService(Context.AUDIO_SERVICE) as android.media.AudioManager
                am.mode = android.media.AudioManager.MODE_IN_COMMUNICATION
                val minBuf = android.media.AudioRecord.getMinBufferSize(
                    48000,
                    android.media.AudioFormat.CHANNEL_IN_MONO,
                    android.media.AudioFormat.ENCODING_PCM_16BIT
                )
                val record = android.media.AudioRecord(
                    android.media.MediaRecorder.AudioSource.VOICE_COMMUNICATION,
                    48000,
                    android.media.AudioFormat.CHANNEL_IN_MONO,
                    android.media.AudioFormat.ENCODING_PCM_16BIT,
                    (minBuf * 2).coerceAtLeast(48000)
                )
                if (record.state != android.media.AudioRecord.STATE_INITIALIZED) {
                    Log.d("Govorilka", "vosk: AudioRecord not initialized")
                    running = false
                    return@Thread
                }
                record.startRecording()
                val buf = ShortArray(4800) // 100ms
                // NOTE: no VAD gate here. Vosk must receive the CONTINUOUS
                // stream — skipping quiet frames between "ба-рон" syllables
                // broke recognition (the wake word was only caught after
                // many repetitions). CPU cost is acceptable for the wake
                // word detector.
                while (running) {
                    val n = record.read(buf, 0, buf.size)
                    if (n > 0) {
                        val pcm = ByteArray(n * 2)
                        for (i in 0 until n) {
                            pcm[i * 2] = (buf[i].toInt() and 0xFF).toByte()
                            pcm[i * 2 + 1] = ((buf[i].toInt() shr 8) and 0xFF).toByte()
                        }
                        if (r.acceptWaveForm(pcm, pcm.size)) {
                            val res = org.json.JSONObject(r.result)
                            val text = res.optString("text", "").lowercase()
                            if (text.contains("барон")) {
                                Log.d("Govorilka", "vosk: wake word heard: $text")
                                running = false
                                onWake?.invoke()
                                break
                            }
                        }
                    }
                }
                record.stop()
                record.release()
                r.close()
                m.close()
                model = null
                rec = null
            } catch (e: Exception) {
                Log.d("Govorilka", "vosk: error: ${e.message}")
            } finally {
                running = false
            }
        }
        thread?.start()
    }

    /** Stop listening. Only signals the worker thread — the worker itself
     * closes native resources (closing Vosk from another thread while the
     * recognizer is running caused a native SIGSEGV). */
    fun stop() {
        running = false
        thread?.interrupt()
        thread = null
    }

    val isRunning: Boolean get() = running
}
