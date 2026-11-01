package resources

const QueuedMedia = "⏳ В очереди: %s..."

const (
	Ready               = "✅ Готово!"
	Silence             = "(тишина)"
	Cancelled           = "❌ Отменено."
	TranscriptionFailed = "Произошла ошибка при расшифровке."
	UploadFailed        = "Не удалось отправить файл на расшифровку."
	MissingJob          = "Задание недоступно в сервисе расшифровки. Отправь файл ещё раз."
	Queued              = "⏳ В очереди..."
	Transcribing        = "⏳ Расшифровываю..."
	Progress            = "⏳ Расшифровываю... %.0f%%"
	CancelDenied        = "Отменить расшифровку может только владелец бота."
)

const CancelButton = "❌ Отменить"
const LectureFileName = "lecture_%d.txt"

const DownloadingMedia = "⏳ Скачиваю %s..."
const UploadingMedia = "⏳ Отправляю на расшифровку..."
const DownloadPercent = "⏳ Скачиваю %s... %.0f%%"
const DownloadBytes = "⏳ Скачиваю %s... %s"
