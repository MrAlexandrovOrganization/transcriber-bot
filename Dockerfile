FROM python:3.11-slim AS proto-builder

RUN pip install --no-cache-dir grpcio-tools==1.62.3

WORKDIR /proto-build
COPY proto/ ./proto/

RUN python -m grpc_tools.protoc \
        -I . \
        --python_out=. \
        --grpc_python_out=. \
        proto/whisper.proto


FROM python:3.11-slim

RUN pip install --no-cache-dir poetry==2.2.1

WORKDIR /app

COPY pyproject.toml poetry.lock* ./

RUN poetry config virtualenvs.create false && \
    poetry install --only main --no-interaction --no-ansi --no-root

COPY bot/ ./bot/
COPY main.py ./
COPY --from=proto-builder /proto-build/proto/ ./proto/

CMD ["python", "main.py"]
