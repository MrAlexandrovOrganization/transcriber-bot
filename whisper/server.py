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

        model_size = os.getenv("WHISPER_MODEL", "small")
        logger.info("Loading Whisper model '%s'...", model_size)
        self._model = WhisperModel(model_size, device="cpu", compute_type="int8")
        logger.info("Whisper model loaded.")

    def Transcribe(self, request_iterator, context):
        tmp_path = None
        try:
            first_chunk = next(request_iterator, None)
            if first_chunk is None or not first_chunk.format:
                context.set_code(grpc.StatusCode.INVALID_ARGUMENT)
                context.set_details("First chunk must contain a non-empty format field")
                return whisper_pb2.TranscribeResponse()

            fmt = first_chunk.format

            with tempfile.NamedTemporaryFile(suffix=f".{fmt}", delete=False) as tmp:
                tmp_path = tmp.name
                tmp.write(first_chunk.data)
                for chunk in request_iterator:
                    tmp.write(chunk.data)

            logger.info("Received audio file: path=%s format=%s", tmp_path, fmt)

            segments, _ = self._model.transcribe(
                tmp_path,
                language="ru",
                beam_size=5,
                vad_filter=True,
                temperature=0.0,
            )
            text = " ".join(seg.text.strip() for seg in segments).strip()
            return whisper_pb2.TranscribeResponse(text=text)

        except Exception as e:
            logger.error("Transcription error: %s", e)
            context.set_code(grpc.StatusCode.INTERNAL)
            context.set_details(str(e))
            return whisper_pb2.TranscribeResponse()

        finally:
            if tmp_path:
                try:
                    os.unlink(tmp_path)
                except OSError:
                    pass
