"""Telegram bot for audio transcription."""

import logging

from telegram import Update
from telegram.ext import Application, CommandHandler, MessageHandler, filters

from bot.config import BOT_TOKEN, LOCAL_API_URL, ROOT_ID
from bot.whisper_client import WhisperUnavailableError, whisper_client

logger = logging.getLogger(__name__)
logging.getLogger("httpx").setLevel(logging.WARNING)


def _authorized(update: Update) -> bool:
    return update.effective_user is not None and update.effective_user.id == ROOT_ID


async def cmd_start(update: Update, context) -> None:
    if not _authorized(update):
        return
    await update.message.reply_text(
        "Привет! Пересылай мне голосовые сообщения, кружочки или видео — я расшифрую их в текст."
    )


async def handle_voice(update: Update, context) -> None:
    if not _authorized(update):
        return

    message = update.effective_message
    if message.voice:
        file = await context.bot.get_file(message.voice.file_id)
        fmt = "ogg"
    elif message.video_note:
        file = await context.bot.get_file(message.video_note.file_id)
        fmt = "mp4"
    elif message.video:
        file = await context.bot.get_file(message.video.file_id)
        fmt = "mp4"
    else:
        # Video sent as a document (without compression)
        doc = message.document
        file = await context.bot.get_file(doc.file_id)
        fmt = doc.file_name.rsplit(".", 1)[-1].lower() if doc.file_name else "mp4"

    status = await message.reply_text("⏳ Транскрибирую...")

    try:
        audio_data = await file.download_as_bytearray()
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
    logger.info("Bot started")
    app.run_polling()
