package com.fucklms.app

import android.content.Context
import android.content.SharedPreferences
import android.os.Handler
import android.os.Looper
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import org.json.JSONArray
import org.json.JSONObject
import java.io.IOException
import java.util.concurrent.TimeUnit

// ===== Настройки сервиса =====

// Адрес и bearer-токен Go-сервиса. Хранятся в отдельном зашифрованном файле
// (secure_service_prefs): прежний secure_accounts_prefs не трогаем — миграций нет.
class ServiceSettings(context: Context) {

    private val prefs: SharedPreferences

    init {
        val masterKey = MasterKey.Builder(context)
            .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
            .build()

        prefs = EncryptedSharedPreferences.create(
            context,
            "secure_service_prefs",
            masterKey,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM
        )
    }

    fun getUrl(): String = prefs.getString("service_url", "") ?: ""

    fun getToken(): String = prefs.getString("service_token", "") ?: ""

    fun isConfigured(): Boolean = getUrl().isNotBlank() && getToken().isNotBlank()

    fun save(url: String, token: String) {
        prefs.edit()
            .putString("service_url", url.trim().trimEnd('/'))
            .putString("service_token", token.trim())
            .apply()
    }
}

// ===== Модели (зеркалят openapi.yaml) =====

// Студент из GET /api/students — пароли сервис не возвращает никогда.
data class Student(
    val id: Long,
    val firstName: String,
    val lastName: String,
    val studyGroup: String,
    val lmsLogin: String,
    val isSelected: Boolean
)

// Одно SSE-событие result из POST /api/mark.
data class MarkEvent(
    val login: String,
    val status: String, // success | already | error
    val message: String
)

// Финальное SSE-событие done.
data class MarkSummary(
    val total: Int,
    val success: Int,
    val already: Int,
    val error: Int
)

// ===== Клиент сервиса =====

// Клиент API Go-сервиса. Все методы асинхронные, колбэки приходят на main-поток.
class ServiceClient(private val settings: ServiceSettings) {

    private val json = "application/json; charset=utf-8".toMediaType()
    private val mainHandler = Handler(Looper.getMainLooper())

    // readTimeout 60 с: SSE-стрим присылает heartbeat каждые ~15 с,
    // поэтому тишина дольше минуты — реальный сбой соединения.
    private val client = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(60, TimeUnit.SECONDS)
        .build()

    private fun Request.Builder.withAuth(): Request.Builder {
        header("Authorization", "Bearer " + settings.getToken())
        return this
    }

    private fun postJson(body: JSONObject, urlPath: String): Request {
        return Request.Builder()
            .url(settings.getUrl() + urlPath)
            .post(body.toString().toRequestBody(json))
            .withAuth()
            .build()
    }

    private fun putJson(body: JSONObject, urlPath: String): Request {
        return Request.Builder()
            .url(settings.getUrl() + urlPath)
            .put(body.toString().toRequestBody(json))
            .withAuth()
            .build()
    }

    // Текст ошибки из ответа сервиса ({"message": ...}) или HTTP-код.
    private fun errorMessage(response: Response): String {
        val body = try { response.body?.string() } catch (_: Exception) { null }
        val message = body?.let {
            try { JSONObject(it).optString("message", "") } catch (_: Exception) { "" }
        }
        return if (!message.isNullOrBlank()) message
        else "Ошибка сервиса: HTTP ${response.code}"
    }

    private fun <T> runAsync(block: () -> T, onResult: (T) -> Unit) {
        Thread {
            val result = try { block() } catch (_: IOException) { null }
            mainHandler.post { result?.let(onResult) }
        }.start()
    }

    // GET /api/students
    fun getStudents(onSuccess: (List<Student>) -> Unit, onError: (String) -> Unit) {
        Thread {
            try {
                val request = Request.Builder()
                    .url(settings.getUrl() + "/api/students")
                    .get()
                    .withAuth()
                    .build()

                client.newCall(request).execute().use { response ->
                    if (!response.isSuccessful) {
                        val message = errorMessage(response)
                        mainHandler.post { onError(message) }
                        return@use
                    }
                    val array = JSONArray(response.body?.string() ?: "[]")
                    val students = mutableListOf<Student>()
                    for (i in 0 until array.length()) {
                        val obj = array.getJSONObject(i)
                        students.add(
                            Student(
                                id = obj.getLong("id"),
                                firstName = obj.getString("firstName"),
                                lastName = obj.getString("lastName"),
                                studyGroup = obj.getString("studyGroup"),
                                lmsLogin = obj.getString("lmsLogin"),
                                isSelected = obj.getBoolean("isSelected")
                            )
                        )
                    }
                    mainHandler.post { onSuccess(students) }
                }
            } catch (e: Exception) {
                mainHandler.post { onError("Нет связи с сервисом: ${e.message}") }
            }
        }.start()
    }

    // POST /api/students
    fun createStudent(
        firstName: String, lastName: String, studyGroup: String,
        lmsLogin: String, lmsPassword: String,
        onDone: (error: String?) -> Unit
    ) {
        runAsync({
            val body = JSONObject()
                .put("firstName", firstName).put("lastName", lastName)
                .put("studyGroup", studyGroup).put("lmsLogin", lmsLogin)
                .put("lmsPassword", lmsPassword)

            client.newCall(postJson(body, "/api/students")).execute().use { response ->
                if (response.isSuccessful) null else errorMessage(response)
            }
        }, onDone)
    }

    // PUT /api/students/{id}; lmsPassword == null → пароль не менять.
    fun updateStudent(
        id: Long,
        firstName: String? = null, lastName: String? = null,
        studyGroup: String? = null, lmsLogin: String? = null,
        lmsPassword: String? = null, isSelected: Boolean? = null,
        onDone: (error: String?) -> Unit
    ) {
        runAsync({
            val body = JSONObject()
            firstName?.let { body.put("firstName", it) }
            lastName?.let { body.put("lastName", it) }
            studyGroup?.let { body.put("studyGroup", it) }
            lmsLogin?.let { body.put("lmsLogin", it) }
            lmsPassword?.let { body.put("lmsPassword", it) }
            isSelected?.let { body.put("isSelected", it) }

            client.newCall(putJson(body, "/api/students/$id")).execute().use { response ->
                if (response.isSuccessful) null else errorMessage(response)
            }
        }, onDone)
    }

    // DELETE /api/students/{id}
    fun deleteStudent(id: Long, onDone: (error: String?) -> Unit) {
        runAsync({
            val request = Request.Builder()
                .url(settings.getUrl() + "/api/students/$id")
                .delete()
                .withAuth()
                .build()

            client.newCall(request).execute().use { response ->
                if (response.isSuccessful) null else errorMessage(response)
            }
        }, onDone)
    }

    // POST /api/mark — читает SSE-поток построчно и раскидывает события в колбэки.
    // studentIds == null → сервис отмечает всех с isSelected = true.
    fun mark(
        qrUrl: String,
        studentIds: List<Long>?,
        onEvent: (MarkEvent) -> Unit,
        onSummary: (MarkSummary) -> Unit,
        onError: (String) -> Unit
    ) {
        Thread {
            try {
                val body = JSONObject().put("qrUrl", qrUrl)
                if (studentIds != null) {
                    body.put("studentIds", JSONArray(studentIds))
                }

                val request = Request.Builder()
                    .url(settings.getUrl() + "/api/mark")
                    .post(body.toString().toRequestBody(json))
                    .header("Accept", "text/event-stream")
                    .withAuth()
                    .build()

                client.newCall(request).execute().use { response ->
                    if (!response.isSuccessful) {
                        val message = errorMessage(response)
                        mainHandler.post { onError(message) }
                        return@use
                    }

                    val source = response.body?.source() ?: run {
                        mainHandler.post { onError("Пустой ответ сервиса") }
                        return@use
                    }

                    var event = ""
                    var data = ""
                    while (true) {
                        val line = source.readUtf8Line() ?: break // поток закрыт
                        when {
                            line.isEmpty() -> {
                                // Пустая строка — конец события, отправляем накопленное.
                                if (event.isNotEmpty() && data.isNotEmpty()) {
                                    dispatchEvent(event, data, onEvent, onSummary)
                                }
                                event = ""
                                data = ""
                            }
                            line.startsWith(":") -> { /* heartbeat-комментарий, игнорируем */ }
                            line.startsWith("event:") -> event = line.removePrefix("event:").trim()
                            line.startsWith("data:") -> data = line.removePrefix("data:").trim()
                        }
                    }
                }
            } catch (e: Exception) {
                mainHandler.post { onError("Соединение с сервисом оборвалось: ${e.message}") }
            }
        }.start()
    }

    private fun dispatchEvent(
        event: String, data: String,
        onEvent: (MarkEvent) -> Unit,
        onSummary: (MarkSummary) -> Unit
    ) {
        try {
            val obj = JSONObject(data)
            when (event) {
                "result" -> {
                    val markEvent = MarkEvent(
                        login = obj.getString("login"),
                        status = obj.getString("status"),
                        message = obj.optString("message", "")
                    )
                    mainHandler.post { onEvent(markEvent) }
                }
                "done" -> {
                    val summary = MarkSummary(
                        total = obj.optInt("total", 0),
                        success = obj.optInt("success", 0),
                        already = obj.optInt("already", 0),
                        error = obj.optInt("error", 0)
                    )
                    mainHandler.post { onSummary(summary) }
                }
            }
        } catch (_: Exception) {
            // Кривое событие не должно рвать весь поток.
        }
    }
}
