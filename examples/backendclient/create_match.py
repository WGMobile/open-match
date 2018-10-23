import grpc
from backend_pb2 import Profile
import backend_pb2_grpc as backend

if __name__ == "__main__":
    channel = grpc.insecure_channel('localhost:50505')
    stub = backend.APIStub(channel)

    profile_name = "testprofile"

    json_data = None
    with open("profiles/%s.json" % (profile_name,), 'r') as file:        
        json_data=file.read()

    if json_data:
        print "creating match for profile: %s" % (profile_name,)

        profile = Profile(id=profile_name, properties=json_data)
        result = stub.CreateMatch(profile)
        print result
