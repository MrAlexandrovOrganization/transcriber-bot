"""Configuration and logging setup for the transcriber bot."""

import logging
import os

from dotenv import load_dotenv

load_dotenv()

BOT_TOKEN: str = os.getenv("BOT_TOKEN", "")
ROOT_ID: int = int(os.getenv("ROOT_ID", "0"))
LOCAL_API_URL: str = os.getenv("TELEGRAM_LOCAL_API_URL", "")

if not BOT_TOKEN:
    raise ValueError("BOT_TOKEN environment variable is not set")
if not ROOT_ID:
    raise ValueError("ROOT_ID environment variable is not set")

logging.basicConfig(
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
    level=logging.INFO,
)
