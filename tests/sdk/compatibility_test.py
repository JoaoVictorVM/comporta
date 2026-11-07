"""Runs the official SDKs against the gateway and the built-in fake provider.

Only the base URL and the API key differ from what an application would use, and no
provider credential or network access is involved.
"""

import base64
import json
import os
import re
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

import anthropic
import openai
import pytest

REPO = Path(__file__).resolve().parents[2]
SYNTHETIC_PROVIDER_KEY = "synthetic-provider-key"
TINY_PNG = base64.b64encode(b"\x89PNG\r\n\x1a\n").decode()
WEATHER_TOOL = {"name": "get_weather", "description": "Weather", "input_schema": {"type": "object", "properties": {}}}

MARKER = os.environ.get("COMPORTA_PRIVACY_MARKER") or "cmpcanary" + secrets.token_hex(16)
USER_MARKER = "cmpuser" + MARKER.removeprefix("cmpcanary")
PROMPTS_SENT = {"count": 0}


def prompt(text):
    """Every prompt of this suite goes through here, so that a run can look for the marker."""
    PROMPTS_SENT["count"] += 1
    return f"{text} {MARKER}"


def assert_no_marker(path):
    data = path.read_bytes()
    escaped = json.dumps(MARKER).strip('"').encode()
    for needle in (MARKER.encode(), USER_MARKER.encode(), escaped):
        # Only the file name is reported, so that the failure does not repeat the content.
        assert needle not in data, f"privacy marker found in {path.name}"


def free_port():
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        return probe.getsockname()[1]


def wait_for_port(port, process):
    deadline = time.time() + 30
    while time.time() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"process exited with {process.returncode}")
        with socket.socket() as probe:
            probe.settimeout(0.2)
            if probe.connect_ex(("127.0.0.1", port)) == 0:
                return
        time.sleep(0.05)
    raise RuntimeError(f"port {port} did not open")


@pytest.fixture(scope="session")
def binary(tmp_path_factory):
    configured = os.environ.get("COMPORTA_BIN")
    if configured:
        return configured
    suffix = ".exe" if sys.platform == "win32" else ""
    target = tmp_path_factory.mktemp("bin") / f"comporta{suffix}"
    subprocess.run(["go", "build", "-o", str(target), "./cmd/comporta"], cwd=REPO, check=True)
    return str(target)


class Gateway:
    def __init__(self, binary, scenario):
        self.dir = tempfile.mkdtemp(prefix="comporta-sdk-")
        self.fake_port, self.proxy_port, self.admin_port = free_port(), free_port(), free_port()
        self.processes = []
        environment = {**os.environ, "COMPORTA_SDK_PROVIDER_KEY": SYNTHETIC_PROVIDER_KEY}
        config = Path(self.dir) / "comporta.yaml"
        config.write_text(
            f"""timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: http://127.0.0.1:{self.fake_port}
    api_key: ${{COMPORTA_SDK_PROVIDER_KEY}}
    models: [fake-claude]
  openai:
    api_format: openai
    base_url: http://127.0.0.1:{self.fake_port}/v1
    api_key: ${{COMPORTA_SDK_PROVIDER_KEY}}
    models: [fake-gpt]
apps:
  sdk-tests:
    allowed_models: [fake-claude, fake-gpt]
listeners:
  proxy: {{port: {self.proxy_port}}}
  admin: {{port: {self.admin_port}}}
""",
            encoding="utf-8",
        )
        created = subprocess.run(
            [binary, "keys", "create", "sdk-tests", "--config", str(config)],
            capture_output=True, text=True, check=True, env=environment,
        )
        self.key = re.search(r"cmp_[0-9A-Za-z]{43}", created.stdout).group(0)
        fake = self.start([binary, "fake-provider", "--port", str(self.fake_port), "--scenario", scenario], environment)
        wait_for_port(self.fake_port, fake)
        serve = self.start([binary, "serve", "--config", str(config)], environment)
        wait_for_port(self.proxy_port, serve)
        wait_for_port(self.admin_port, serve)

    def start(self, command, environment):
        output = open(Path(self.dir) / f"process-{len(self.processes)}.log", "wb")
        process = subprocess.Popen(command, env=environment, stdout=output, stderr=subprocess.STDOUT)
        output.close()
        self.processes.append(process)
        return process

    def stop(self):
        for process in self.processes:
            process.terminate()
        for process in self.processes:
            process.wait(timeout=20)
        try:
            outputs = [path for path in Path(self.dir).rglob("*") if path.is_file()]
            assert any(path.suffix == ".log" for path in outputs), "no process output to inspect"
            for path in outputs:
                assert_no_marker(path)
        finally:
            shutil.rmtree(self.dir, ignore_errors=True)

    def anthropic(self, **options):
        return anthropic.Anthropic(base_url=f"http://127.0.0.1:{self.proxy_port}", api_key=self.key, max_retries=0, **options)

    def openai(self, **options):
        return openai.OpenAI(base_url=f"http://127.0.0.1:{self.proxy_port}/v1", api_key=self.key, max_retries=0, **options)


@pytest.fixture
def make_gateway(binary):
    started = []

    def make(scenario="success"):
        gateway = Gateway(binary, scenario)
        started.append(gateway)
        return gateway

    yield make
    for gateway in started:
        gateway.stop()


@pytest.fixture
def gateway(make_gateway):
    return make_gateway()


@pytest.fixture(scope="session", autouse=True)
def prompts_were_sent():
    yield
    assert PROMPTS_SENT["count"] > 0, "the suite sent no marked prompt, so its scan proved nothing"


def test_anthropic_sync_and_stream(gateway):
    client = gateway.anthropic()
    message = client.messages.create(
        model="fake-claude",
        max_tokens=32,
        messages=[{
            "role": "user",
            "content": [
                {"type": "text", "text": prompt("synthetic prompt")},
                {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": TINY_PNG}},
            ],
        }],
        tools=[WEATHER_TOOL],
    )
    assert message.content[0].text == "Olá"
    assert message.usage.input_tokens == 10
    assert message.usage.output_tokens == 2

    with client.messages.stream(model="fake-claude", max_tokens=32, messages=[{"role": "user", "content": prompt("synthetic")}]) as stream:
        text = "".join(stream.text_stream)
        final = stream.get_final_message()
    assert text == "Olá mundo"
    assert final.usage.output_tokens == 2


def test_openai_sync_and_stream(gateway):
    client = gateway.openai()
    completion = client.chat.completions.create(
        model="fake-gpt",
        messages=[{
            "role": "user",
            "content": [
                {"type": "text", "text": prompt("synthetic prompt")},
                {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{TINY_PNG}"}},
            ],
        }],
        tools=[{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object", "properties": {}}}}],
        temperature=None,
    )
    assert completion.choices[0].message.content == "Olá"
    assert completion.usage.total_tokens == 12

    chunks = list(client.chat.completions.create(model="fake-gpt", stream=True, messages=[{"role": "user", "content": prompt("synthetic")}]))
    text = "".join(chunk.choices[0].delta.content or "" for chunk in chunks if chunk.choices)
    assert text == "Olá mundo"
    assert all(chunk.usage is None for chunk in chunks), "the client did not ask for the usage chunk"

    asked = list(client.chat.completions.create(
        model="fake-gpt", stream=True, stream_options={"include_usage": True}, messages=[{"role": "user", "content": prompt("synthetic")}],
    ))
    assert asked[-1].usage.prompt_tokens == 10
    assert asked[-1].choices == []


def test_models_both_sdks(gateway):
    assert [model.id for model in gateway.anthropic().models.list()] == ["fake-claude"]
    assert [model.id for model in gateway.openai().models.list()] == ["fake-gpt"]


def test_anthropic_count_tokens(gateway):
    counted = gateway.anthropic().messages.count_tokens(model="fake-claude", messages=[{"role": "user", "content": prompt("synthetic")}])
    assert counted.input_tokens == 10


def test_stream_failure_is_not_success(make_gateway):
    gateway = make_gateway("disconnect-midstream")
    with pytest.raises(anthropic.APIError):
        with gateway.anthropic().messages.stream(model="fake-claude", max_tokens=8, messages=[{"role": "user", "content": prompt("x")}]) as stream:
            list(stream.text_stream)
    with pytest.raises(openai.APIError):
        list(gateway.openai().chat.completions.create(model="fake-gpt", stream=True, messages=[{"role": "user", "content": prompt("x")}]))


def test_provider_errors_reach_the_sdks_as_their_own_types(make_gateway):
    gateway = make_gateway("rate-limit")
    with pytest.raises(anthropic.RateLimitError) as caught:
        gateway.anthropic().messages.create(model="fake-claude", max_tokens=8, messages=[{"role": "user", "content": prompt("x")}])
    assert caught.value.body["error"]["comporta_code"] == "provider_rate_limited"
    with pytest.raises(openai.RateLimitError):
        gateway.openai().chat.completions.create(model="fake-gpt", messages=[{"role": "user", "content": prompt("x")}])


def test_anthropic_identity_header_and_metadata(gateway):
    with_header = gateway.anthropic(default_headers={"X-Comporta-User": USER_MARKER})
    message = with_header.messages.create(model="fake-claude", max_tokens=8, messages=[{"role": "user", "content": prompt("synthetic")}])
    assert message.content[0].text == "Olá"

    in_body = gateway.anthropic().messages.create(
        model="fake-claude", max_tokens=8, metadata={"user_id": USER_MARKER}, messages=[{"role": "user", "content": prompt("synthetic")}],
    )
    assert in_body.content[0].text == "Olá"

    counted = with_header.messages.count_tokens(model="fake-claude", messages=[{"role": "user", "content": prompt("synthetic")}])
    assert counted.input_tokens == 10

    rejected = gateway.anthropic(default_headers={"X-Comporta-User": f"{USER_MARKER}@mail.example"})
    with pytest.raises(anthropic.BadRequestError) as caught:
        rejected.messages.create(model="fake-claude", max_tokens=8, messages=[{"role": "user", "content": prompt("synthetic")}])
    assert caught.value.body["error"]["comporta_code"] == "user_email_not_allowed"
    assert USER_MARKER not in str(caught.value.body)


def test_openai_identity_header_and_user(gateway):
    with_header = gateway.openai(default_headers={"X-Comporta-User": USER_MARKER})
    completion = with_header.chat.completions.create(model="fake-gpt", messages=[{"role": "user", "content": prompt("synthetic")}])
    assert completion.choices[0].message.content == "Olá"

    in_body = gateway.openai().chat.completions.create(model="fake-gpt", user=USER_MARKER, messages=[{"role": "user", "content": prompt("synthetic")}])
    assert in_body.choices[0].message.content == "Olá"

    rejected = gateway.openai(default_headers={"X-Comporta-User": "52998224725"})
    with pytest.raises(openai.BadRequestError) as caught:
        rejected.chat.completions.create(model="fake-gpt", messages=[{"role": "user", "content": prompt("synthetic")}])
    assert caught.value.body["comporta_code"] == "user_document_not_allowed"


def test_identity_streaming_compatibility(gateway):
    client = gateway.anthropic(default_headers={"X-Comporta-User": USER_MARKER})
    with client.messages.stream(model="fake-claude", max_tokens=8, messages=[{"role": "user", "content": prompt("synthetic")}]) as stream:
        assert "".join(stream.text_stream) == "Olá mundo"

    chunks = list(gateway.openai().chat.completions.create(
        model="fake-gpt", stream=True, user=USER_MARKER, messages=[{"role": "user", "content": prompt("synthetic")}],
    ))
    assert "".join(chunk.choices[0].delta.content or "" for chunk in chunks if chunk.choices) == "Olá mundo"
    assert all(chunk.usage is None for chunk in chunks), "the client did not ask for the usage chunk"


def test_blocked_endpoint_returns_404(gateway):
    request = urllib.request.Request(f"http://127.0.0.1:{gateway.proxy_port}/v1/files", method="POST", data=b"x")
    with pytest.raises(urllib.error.HTTPError) as caught:
        urllib.request.urlopen(request, timeout=5)
    assert caught.value.code == 404
    assert b"endpoint not allowed by Comporta" in caught.value.read()
