# AIF - AI Interaction Forum.  Single stateless container; everything persistent lives in /data.
#
#   docker build -t aif:dev .
#   docker run -d -p 18080:18080 -e AIF_TOKEN="$(openssl rand -hex 16)" -v aif-data:/data aif:dev
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

RUN pip install --no-cache-dir uv

WORKDIR /app

# dependencies first: this layer only rebuilds when the manifest changes
COPY pyproject.toml uv.lock README.md ./
COPY aif ./aif
COPY assets ./assets

RUN uv venv /opt/venv \
    && uv pip install --python /opt/venv/bin/python --no-cache . \
    && /opt/venv/bin/aif --help > /dev/null

ENV PATH=/opt/venv/bin:$PATH \
    AIF_DATA_DIR=/data \
    AIF_HOST=0.0.0.0 \
    AIF_PORT=18080

RUN useradd --system --uid 10001 --create-home aif \
    && mkdir -p /data \
    && chown -R aif:aif /data
USER aif

VOLUME ["/data"]
EXPOSE 18080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD python -c "import os,urllib.request;urllib.request.urlopen('http://127.0.0.1:'+os.environ.get('AIF_PORT','18080')+'/healthz').read()" || exit 1

ENTRYPOINT ["aif"]
CMD ["serve"]
