using Conformance;

namespace Conformance.Tests;

[TestClass]
public sealed class ConformanceMessageTests
{
    [TestMethod]
    public void BuildReturnsExpectedMessage()
    {
        Assert.AreEqual(
            "ActionsCommunity/multirunner:windows:ok",
            ConformanceMessage.Build("ActionsCommunity/multirunner", "windows"));
    }

    [TestMethod]
    public void BuildRejectsBlankRepository()
    {
        Assert.ThrowsExactly<ArgumentException>(() => ConformanceMessage.Build("", "windows"));
    }
}
