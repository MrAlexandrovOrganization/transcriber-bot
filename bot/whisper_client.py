"""gRPC client for the Whisper transcription service."""

import logging
import os
from collections.abc import Iterator

import grpc

from proto import whisper_pb2, whisper_pb2_grpc

logger = logging.getLogger(__name__)

_CHUNK_SIZE = 1 * 1024 * 1024  # 1 MB per chunk
_UNAVAILABLE_CODES = (grpc.StatusCode.UNAVAILABLE, grpc.StatusCode.DEADLINE_EXCEEDED)


class WhisperUnavailableError(Exception):
    """Raised when the whisper service cannot be reached."""


def _stream_chunks(audio_data: bytes, fmt: str) -> Iterator[whisper_pb2.TranscribeChunk]:
    first = True
    for i in range(0, len(audio_data), _CHUNK_SIZE):
        yield whisper_pb2.TranscribeChunk(
            data=audio_data[i : i + _CHUNK_SIZE],
            format=fmt if first else "",
        )
        first = False


class WhisperClient:
    def __init__(self, host: str, port: str):
        self._channel = grpc.insecure_channel(f"{host}:{port}")
        self._stub = whisper_pb2_grpc.TranscriptionServiceStub(self._channel)

    def transcribe(self, audio_data: bytes, fmt: str) -> str:
        try:
            response = self._stub.Transcribe(
                _stream_chunks(audio_data, fmt),
                timeout=120,
            )
            return response.text
        except grpc.RpcError as e:
            if e.code() in _UNAVAILABLE_CODES:
                raise WhisperUnavailableError() from e
            raise


whisper_client = WhisperClient(
    os.getenv("WHISPER_GRPC_HOST", "localhost"),
    os.getenv("WHISPER_GRPC_PORT", "50053"),
)
