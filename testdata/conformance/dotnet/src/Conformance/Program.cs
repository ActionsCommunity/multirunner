namespace Conformance;

public static class ConformanceMessage
{
    public static string Build(string repository, string platform)
    {
        ArgumentException.ThrowIfNullOrWhiteSpace(repository);
        ArgumentException.ThrowIfNullOrWhiteSpace(platform);
        return $"{repository}:{platform}:ok";
    }
}

public static class Program
{
    public static void Main()
    {
        Console.WriteLine(ConformanceMessage.Build("ActionsCommunity/multirunner", "windows"));
    }
}
