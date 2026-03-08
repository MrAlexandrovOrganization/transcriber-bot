"""gRPC server for the Whisper transcription service."""

import logging
import os
import tempfile

import grpc

from proto import whisper_pb2, whisper_pb2_grpc

logger = logging.getLogger(__name__)


class TranscriptionServicer(whisper_pb2_grpc.TranscriptionServiceServicer):
    def __init__(self):
        from faster_whisper import WhisperModel

        model_size = os.getenv("WHISPER_MODEL", "base")
        logger.info(f"Loading Whisper model '{model_size}'...")
        self._model = WhisperModel(model_size, device="cpu", compute_type="int8")
        logger.info("Whisper model loaded.")

    def Transcribe(self, request, context):
        tmp_path = None
        try:
            model = self._model
            with tempfile.NamedTemporaryFile(
                suffix=f".{request.format}", delete=False
            ) as tmp:
                tmp.write(request.audio_data)
                tmp_path = tmp.name

            segments, _ = model.transcribe(
                tmp_path,
                language="ru",
                beam_size=5,
                vad_filter=True,
                temperature=0.0,
            )
            text = " ".join(seg.text.strip() for seg in segments).strip()
            return whisper_pb2.TranscribeResponse(text=text)
        except Exception as e:
            logger.error(f"Transcription error: {e}")
            context.set_code(grpc.StatusCode.INTERNAL)
            context.set_details(str(e))
            return whisper_pb2.TranscribeResponse()
        finally:
            if tmp_path:
                import os as _os

                try:
                    _os.unlink(tmp_path)
                except OSError:
                    pass
