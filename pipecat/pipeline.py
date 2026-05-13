from pipecat.pipeline import Pipeline
from pipecat.transports import BaseTransport

from services.vad import SileroVAD
from services.sarvaam_sst import SarvaamSSTService
from services.sarvaam_tts import SarvaamTTSService
from services.sentiment_classifier import SentimentClassifier


def create_pipeline(transport: BaseTransport) -> Pipeline:
    return Pipeline([
        transport,
        SileroVAD(),
        SarvaamSSTService(),
        SentimentClassifier(),
        SarvaamTTSService(),
        transport,
    ])
