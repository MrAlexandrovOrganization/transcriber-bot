"""Telegram bot for audio transcription."""

import logging

from telegram import Update
from telegram.ext import Application, CommandHandler, ContextTypes, MessageHandler, filters

from bot.config import BOT_TOKEN, LOCAL_API_URL, ROOT_ID
from bot.whisper_client import WhisperUnavailableError, whisper_client

logger = logging.getLogger(__name__)
logging.getLogger("httpx").setLevel(logging.WARNING)


def _authorized(update: Update) -> bool:
    user = update.effective_user
    if user is None:
        logger.warning("Update has no effective_user: update_id=%s", update.update_id)
        return False
    authorized = user.id == ROOT_ID
    if not authorized:
        logger.warning("Unauthorized access from user_id=%s", user.id)
    return authorized


async def log_all_messages(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    """Catch-all handler to log every incoming message for debugging."""
    msg = update.effective_message
    if msg is None:
        logger.info("Update with no message: update_id=%s", update.update_id)
        return
    logger.info(
        "Incoming message: update_id=%s user_id=%s has_voice=%s has_video_note=%s "
        "has_video=%s has_document=%s has_animation=%s has_text=%s",
        update.update_id,
        update.effective_user.id if update.effective_user else None,
        bool(msg.voice),
        bool(msg.video_note),
        bool(msg.video),
        bool(msg.document),
        bool(msg.animation),
        bool(msg.text),
    )
    if msg.document:
        logger.info(
            "Document: mime_type=%s file_name=%s file_size=%s",
            msg.document.mime_type,
            msg.document.file_name,
            msg.document.file_size,
        )
    if msg.video:
        logger.info(
            "Video: file_size=%s duration=%s mime_type=%s",
            msg.video.file_size,
            msg.video.duration,
            msg.video.mime_type,
        )


async def cmd_start(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if not _authorized(update):
        return
    if update.message is None:
        return
    await update.message.reply_text(
        "Привет! Пересылай мне голосовые сообщения, кружочки или видео — я расшифрую их в текст."
    )


async def handle_voice(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if not _authorized(update):
        return

    message = update.effective_message
    if message is None:
        return
    logger.info(
        "handle_voice triggered: voice=%s video_note=%s video=%s document=%s",
        bool(message.voice),
        bool(message.video_note),
        bool(message.video),
        bool(message.document),
    )
    status = await message.reply_text("⏳ Транскрибирую...")

    file = None
    fmt = None
    try:
        if message.voice:
            file = await context.bot.get_file(message.voice.file_id)
            fmt = "ogg"
        elif message.video_note:
            file = await context.bot.get_file(message.video_note.file_id)
            fmt = "mp4"
        elif message.video:
            logger.info("Downloading video file_id=%s", message.video.file_id)
            file = await context.bot.get_file(message.video.file_id)
            fmt = "mp4"
        else:
            # Video sent as a document (without compression)
            doc = message.document
            if doc != None:
                logger.info("Downloading document file_id=%s mime_type=%s", doc.file_id, doc.mime_type)
                file = await context.bot.get_file(doc.file_id)
                fmt = doc.file_name.rsplit(".", 1)[-1].lower() if doc.file_name else "mp4"

        if file is None or fmt is None:
            return
        logger.info("File fetched, downloading audio data...")
        audio_data = await file.download_as_bytearray()
        logger.info("Downloaded %d bytes, sending to whisper...", len(audio_data))
        text = whisper_client.transcribe(bytes(audio_data), fmt)
        if text:
            await status.edit_text(text)
        else:
            await status.edit_text("(тишина)")
    except WhisperUnavailableError:
        await status.edit_text("Сервис транскрипции недоступен, попробуй позже.")
    except Exception:
        logger.exception("Unexpected error during transcription")
        await status.edit_text("Произошла ошибка при транскрипции.")


def main() -> None:
    builder = Application.builder().token(BOT_TOKEN)
    if LOCAL_API_URL:
        builder = builder.base_url(f"{LOCAL_API_URL}/bot").local_mode(True)
        logger.info("Using local Telegram Bot API server: %s", LOCAL_API_URL)
    app = builder.build()
    app.add_handler(CommandHandler("start", cmd_start))
    app.add_handler(
        MessageHandler(
            filters.VOICE | filters.VIDEO_NOTE | filters.VIDEO | filters.Document.VIDEO,
            handle_voice,
        )
    )
    # Catch-all in a separate group: runs independently of group=0, logs every incoming message
    app.add_handler(MessageHandler(filters.ALL, log_all_messages), group=1)
    logger.info("Bot started")
    app.run_polling()
