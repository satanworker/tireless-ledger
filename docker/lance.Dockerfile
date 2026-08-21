FROM python:3.12-slim
WORKDIR /app
COPY docker/lance/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY docker/lance/server.py .
EXPOSE 8080
CMD ["python", "-u", "server.py"]
