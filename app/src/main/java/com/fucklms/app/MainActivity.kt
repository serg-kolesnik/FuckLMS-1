package com.fucklms.app

import android.Manifest
import android.annotation.SuppressLint
import android.content.pm.PackageManager
import android.os.Bundle
import android.util.Log
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.camera.core.CameraSelector
import androidx.camera.core.ExperimentalGetImage
import androidx.camera.core.ImageAnalysis
import androidx.camera.core.Preview
import androidx.camera.lifecycle.ProcessCameraProvider
import androidx.camera.view.PreviewView
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.ContextCompat
import com.google.mlkit.vision.barcode.BarcodeScannerOptions
import com.google.mlkit.vision.barcode.BarcodeScanning
import com.google.mlkit.vision.barcode.common.Barcode
import com.google.mlkit.vision.common.InputImage
import java.util.concurrent.Executors

class MainActivity : ComponentActivity() {

    private lateinit var serviceSettings: ServiceSettings

    private val cameraPermissionLauncher = registerForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { _ -> }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        serviceSettings = ServiceSettings(this)

        if (ContextCompat.checkSelfPermission(this, Manifest.permission.CAMERA) != PackageManager.PERMISSION_GRANTED) {
            cameraPermissionLauncher.launch(Manifest.permission.CAMERA)
        }

        setContent {
            MaterialTheme {
                MainAppScreen(serviceSettings)
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun MainAppScreen(serviceSettings: ServiceSettings) {
    var selectedTab by remember { mutableIntStateOf(0) }
    var scannedUrl by remember { mutableStateOf("") }

    Scaffold(
        topBar = {
            TopAppBar(title = {
                Text(
                    when (selectedTab) {
                        0 -> "Сканер LMS"
                        1 -> "Отметка посещаемости"
                        else -> "Студенты"
                    }
                )
            })
        },
        bottomBar = {
            NavigationBar {
                NavigationBarItem(
                    selected = selectedTab == 0,
                    onClick = { selectedTab = 0 },
                    icon = { Text("📷") },
                    label = { Text("Сканер") }
                )
                NavigationBarItem(
                    selected = selectedTab == 1,
                    onClick = { selectedTab = 1 },
                    icon = { Text("✅") },
                    label = { Text("Отметка") }
                )
                NavigationBarItem(
                    selected = selectedTab == 2,
                    onClick = { selectedTab = 2 },
                    icon = { Text("👤") },
                    label = { Text("Аккаунты") }
                )
            }
        }
    ) { paddingValues ->
        Box(modifier = Modifier.padding(paddingValues)) {
            when (selectedTab) {
                0 -> ScannerScreen(
                    onQrScanned = { url ->
                        scannedUrl = url
                        selectedTab = 1
                    }
                )
                1 -> MarkingScreen(serviceSettings, scannedUrl)
                2 -> AccountsScreen(serviceSettings)
            }
        }
    }
}

@OptIn(ExperimentalGetImage::class)
@Composable
fun ScannerScreen(onQrScanned: (String) -> Unit) {
    val context = LocalContext.current
    val cameraProviderFuture = remember { ProcessCameraProvider.getInstance(context) }
    val executor = remember { Executors.newSingleThreadExecutor() }

    Box(modifier = Modifier.fillMaxSize()) {
        AndroidView(
            factory = { ctx ->
                val previewView = PreviewView(ctx)
                cameraProviderFuture.addListener({
                    val cameraProvider = cameraProviderFuture.get()
                    val preview = Preview.Builder().build().also { it.setSurfaceProvider(previewView.surfaceProvider) }

                    val scanner = BarcodeScanning.getClient(
                        BarcodeScannerOptions.Builder().setBarcodeFormats(Barcode.FORMAT_QR_CODE).build()
                    )

                    val imageAnalysis = ImageAnalysis.Builder()
                        .setBackpressureStrategy(ImageAnalysis.STRATEGY_KEEP_ONLY_LATEST)
                        .build()

                    imageAnalysis.setAnalyzer(executor) { imageProxy ->
                        val mediaImage = imageProxy.image
                        if (mediaImage != null) {
                            val image = InputImage.fromMediaImage(mediaImage, imageProxy.imageInfo.rotationDegrees)
                            scanner.process(image)
                                .addOnSuccessListener { barcodes ->
                                    for (barcode in barcodes) {
                                        barcode.rawValue?.let { url -> onQrScanned(url) }
                                    }
                                }
                                .addOnCompleteListener { imageProxy.close() }
                        } else {
                            imageProxy.close()
                        }
                    }

                    try {
                        cameraProvider.unbindAll()
                        cameraProvider.bindToLifecycle(
                            context as ComponentActivity,
                            CameraSelector.DEFAULT_BACK_CAMERA,
                            preview,
                            imageAnalysis
                        )
                    } catch (exc: Exception) {
                        Log.e("MoodleApp", "Camera bind failed", exc)
                    }
                }, ContextCompat.getMainExecutor(ctx))
                previewView
            },
            modifier = Modifier.fillMaxSize()
        )
    }
}

// ================== ОТМЕТКА ==================

@Composable
fun MarkingScreen(serviceSettings: ServiceSettings, qrUrl: String) {
    // Результаты приходят SSE-потоком по мере готовности каждого студента.
    val results = remember { mutableStateListOf<MarkEvent>() }
    var summary by remember { mutableStateOf<MarkSummary?>(null) }
    var errorText by remember { mutableStateOf<String?>(null) }
    var running by remember { mutableStateOf(false) }
    // URL, для которого отметка уже запускалась: не рестартовать при рекомпозиции.
    var startedFor by remember { mutableStateOf<String?>(null) }

    fun startMarking() {
        if (running || qrUrl.isEmpty()) return
        if (!serviceSettings.isConfigured()) {
            errorText = "Укажите адрес и токен сервиса во вкладке «Аккаунты»"
            return
        }
        results.clear()
        summary = null
        errorText = null
        running = true
        ServiceClient(serviceSettings).mark(
            qrUrl = qrUrl,
            studentIds = null, // null = все студенты, выбранные галочками
            onEvent = { results.add(it) },
            onSummary = {
                summary = it
                running = false
            },
            onError = {
                errorText = it
                running = false
            }
        )
    }

    LaunchedEffect(qrUrl) {
        if (qrUrl.isNotEmpty() && startedFor != qrUrl) {
            startedFor = qrUrl
            startMarking()
        }
    }

    Column(modifier = Modifier.fillMaxSize().padding(16.dp)) {
        if (qrUrl.isEmpty()) {
            Box(modifier = Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
                Text(
                    "Отсканируйте QR-код",
                    style = MaterialTheme.typography.bodyLarge
                )
            }
            return
        }

        Text(
            text = "QR-ссылка:",
            style = MaterialTheme.typography.titleSmall
        )
        Text(
            text = qrUrl,
            style = MaterialTheme.typography.bodySmall,
            maxLines = 2,
            overflow = TextOverflow.Ellipsis,
            color = MaterialTheme.colorScheme.onSurfaceVariant
        )

        Spacer(modifier = Modifier.height(12.dp))

        when {
            running -> {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    CircularProgressIndicator(modifier = Modifier.size(20.dp), strokeWidth = 2.dp)
                    Spacer(modifier = Modifier.width(12.dp))
                    Text("Отмечаю… готово: ${results.size}", style = MaterialTheme.typography.bodyLarge)
                }
            }
            summary != null -> {
                val s = summary!!
                Text(
                    text = "Готово: ${s.success} отмечено, ${s.already} уже были отмечены, ${s.error} ошибок",
                    style = MaterialTheme.typography.bodyLarge,
                    color = if (s.error == 0) Color.Green else MaterialTheme.colorScheme.error
                )
                Spacer(modifier = Modifier.height(8.dp))
                Button(onClick = { startMarking() }) { Text("Отметить снова") }
            }
            errorText != null -> {
                Text(errorText!!, color = Color.Red)
                Spacer(modifier = Modifier.height(8.dp))
                Button(onClick = { startMarking() }) { Text("Повторить") }
            }
        }

        if (results.isNotEmpty()) {
            HorizontalDivider(modifier = Modifier.padding(vertical = 12.dp))
            LazyColumn {
                items(results) { event ->
                    Card(modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp)) {
                        Row(
                            modifier = Modifier.padding(12.dp).fillMaxWidth(),
                            horizontalArrangement = Arrangement.SpaceBetween,
                            verticalAlignment = Alignment.CenterVertically
                        ) {
                            Text(event.login, style = MaterialTheme.typography.bodyLarge)
                            Text(
                                text = event.message,
                                style = MaterialTheme.typography.bodyMedium,
                                color = when (event.status) {
                                    "success" -> Color.Green
                                    "already" -> MaterialTheme.colorScheme.primary
                                    else -> Color.Red
                                }
                            )
                        }
                    }
                }
            }
        }
    }
}

// ================== АККАУНТЫ (СТУДЕНТЫ) ==================

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AccountsScreen(serviceSettings: ServiceSettings) {
    var students by remember { mutableStateOf<List<Student>?>(null) }
    var loadError by remember { mutableStateOf<String?>(null) }
    var showSettings by remember { mutableStateOf(!serviceSettings.isConfigured()) }
    // Диалог: null = закрыт; Student = редактирование; "new" = добавление нового.
    var editTarget by remember { mutableStateOf<Any?>(null) }
    var busy by remember { mutableStateOf(false) }

    fun reload() {
        ServiceClient(serviceSettings).getStudents(
            onSuccess = {
                students = it
                loadError = null
            },
            onError = {
                loadError = it
                students = null
            }
        )
    }

    LaunchedEffect(Unit) {
        if (serviceSettings.isConfigured()) reload()
    }

    Column(modifier = Modifier.fillMaxSize().padding(16.dp)) {
        // --- Настройки сервиса: адрес + токен (скрыты, если уже сконфигурировано) ---
        if (showSettings) {
            SettingsCard(
                settings = serviceSettings,
                onSaved = {
                    showSettings = false
                    reload()
                }
            )
            HorizontalDivider(modifier = Modifier.padding(vertical = 12.dp))
        } else {
            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.SpaceBetween,
                verticalAlignment = Alignment.CenterVertically
            ) {
                Text("Студенты на сервисе (${students?.size ?: "…"})", style = MaterialTheme.typography.titleMedium)
                Row {
                    IconButton(onClick = { showSettings = true }) {
                        Icon(Icons.Default.Settings, contentDescription = "Настройки сервиса")
                    }
                    IconButton(onClick = { editTarget = "new" }) {
                        Icon(Icons.Default.Edit, contentDescription = "Добавить студента")
                    }
                }
            }
        }

        when {
            !serviceSettings.isConfigured() && !showSettings -> {
                Text("Сервис не настроен", color = MaterialTheme.colorScheme.error)
            }
            loadError != null -> {
                Text(loadError!!, color = Color.Red)
                Spacer(modifier = Modifier.height(8.dp))
                Button(onClick = { reload() }) { Text("Повторить") }
            }
            students == null && serviceSettings.isConfigured() && !showSettings -> {
                CircularProgressIndicator()
            }
            students != null -> {
                LazyColumn {
                    items(students!!) { student ->
                        Card(modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp)) {
                            Row(
                                modifier = Modifier.padding(start = 16.dp).fillMaxWidth(),
                                horizontalArrangement = Arrangement.SpaceBetween,
                                verticalAlignment = Alignment.CenterVertically
                            ) {
                                Column(modifier = Modifier.weight(1f)) {
                                    Text(
                                        "${student.lastName} ${student.firstName}, ${student.studyGroup}",
                                        style = MaterialTheme.typography.bodyLarge
                                    )
                                    Text(
                                        student.lmsLogin,
                                        style = MaterialTheme.typography.bodySmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant
                                    )
                                }
                                Checkbox(
                                    checked = student.isSelected,
                                    onCheckedChange = { checked ->
                                        busy = true
                                        ServiceClient(serviceSettings).updateStudent(
                                            id = student.id, isSelected = checked,
                                            onDone = { error ->
                                                busy = false
                                                if (error != null) loadError = error else reload()
                                            }
                                        )
                                    }
                                )
                                IconButton(
                                    enabled = !busy,
                                    onClick = {
                                        busy = true
                                        ServiceClient(serviceSettings).deleteStudent(
                                            id = student.id,
                                            onDone = { error ->
                                                busy = false
                                                if (error != null) loadError = error else reload()
                                            }
                                        )
                                    }
                                ) {
                                    Icon(Icons.Default.Delete, contentDescription = "Удалить", tint = Color.Red)
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    if (editTarget != null) {
        StudentDialog(
            initial = editTarget as? Student,
            onDismiss = { editTarget = null },
            onSave = { firstName, lastName, group, login, password ->
                busy = true
                val client = ServiceClient(serviceSettings)
                val target = editTarget
                val onSaved: (String?) -> Unit = { error ->
                    busy = false
                    editTarget = null
                    if (error != null) loadError = error
                    reload()
                }
                if (target is Student) {
                    client.updateStudent(
                        id = target.id,
                        firstName = firstName, lastName = lastName, studyGroup = group, lmsLogin = login,
                        // Пустое поле пароля = пароль не менять
                        lmsPassword = password.ifBlank { null },
                        onDone = onSaved
                    )
                } else {
                    client.createStudent(firstName, lastName, group, login, password, onDone = onSaved)
                }
            }
        )
    }
}

// Карточка настроек: адрес сервиса + bearer-токен.
@Composable
fun SettingsCard(settings: ServiceSettings, onSaved: () -> Unit) {
    var url by remember { mutableStateOf(settings.getUrl()) }
    var token by remember { mutableStateOf(settings.getToken()) }
    var errorText by remember { mutableStateOf<String?>(null) }

    Column {
        Text("Настройки сервиса", style = MaterialTheme.typography.titleMedium)
        Spacer(modifier = Modifier.height(4.dp))
        OutlinedTextField(
            value = url,
            onValueChange = { url = it },
            label = { Text("Адрес сервиса (например, http://192.168.1.10:8080)") },
            modifier = Modifier.fillMaxWidth(),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
            singleLine = true
        )
        OutlinedTextField(
            value = token,
            onValueChange = { token = it },
            label = { Text("Токен доступа") },
            modifier = Modifier.fillMaxWidth(),
            visualTransformation = PasswordVisualTransformation(),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
            singleLine = true
        )
        if (errorText != null) {
            Text(errorText!!, color = Color.Red, modifier = Modifier.padding(top = 4.dp))
        }
        Button(
            onClick = {
                if (url.isBlank() || token.isBlank()) {
                    errorText = "Введите адрес и токен"
                } else {
                    settings.save(url, token)
                    onSaved()
                }
            },
            modifier = Modifier.fillMaxWidth().padding(vertical = 8.dp)
        ) { Text("Сохранить") }
    }
}

// Диалог добавления/редактирования студента. При редактировании пустой пароль
// означает «пароль не менять» — сервис не возвращает пароли, показать их нельзя.
@Composable
fun StudentDialog(
    initial: Student?,
    onDismiss: () -> Unit,
    onSave: (firstName: String, lastName: String, group: String, login: String, password: String) -> Unit
) {
    var firstName by remember { mutableStateOf(initial?.firstName ?: "") }
    var lastName by remember { mutableStateOf(initial?.lastName ?: "") }
    var group by remember { mutableStateOf(initial?.studyGroup ?: "") }
    var login by remember { mutableStateOf(initial?.lmsLogin ?: "") }
    var password by remember { mutableStateOf("") }
    var errorText by remember { mutableStateOf<String?>(null) }

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(if (initial == null) "Добавить студента" else "Редактировать студента") },
        text = {
            Column {
                OutlinedTextField(
                    value = lastName, onValueChange = { lastName = it },
                    label = { Text("Фамилия") }, singleLine = true
                )
                OutlinedTextField(
                    value = firstName, onValueChange = { firstName = it },
                    label = { Text("Имя") }, singleLine = true
                )
                OutlinedTextField(
                    value = group, onValueChange = { group = it },
                    label = { Text("Группа") }, singleLine = true
                )
                OutlinedTextField(
                    value = login, onValueChange = { login = it },
                    label = { Text("Логин LMS") }, singleLine = true
                )
                OutlinedTextField(
                    value = password, onValueChange = { password = it },
                    label = { Text(if (initial == null) "Пароль LMS" else "Пароль LMS (пусто — не менять)") },
                    visualTransformation = PasswordVisualTransformation(),
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
                    singleLine = true
                )
                if (errorText != null) {
                    Text(errorText!!, color = Color.Red, modifier = Modifier.padding(top = 4.dp))
                }
            }
        },
        confirmButton = {
            TextButton(onClick = {
                if (lastName.isBlank() || firstName.isBlank() || group.isBlank() ||
                    login.isBlank() || (initial == null && password.isBlank())
                ) {
                    errorText = "Заполните все поля" + if (initial != null) " (пароль можно не менять)" else ""
                } else {
                    onSave(firstName.trim(), lastName.trim(), group.trim(), login.trim(), password)
                }
            }) { Text("Сохранить") }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Отмена") }
        }
    )
}
